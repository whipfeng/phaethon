package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"phaethon/reverse"
)

func main() {
	fmt.Println("=== Control Channel Test ===")
	fmt.Println()

	registryAddr := "127.0.0.1:19901"

	// Step 1: Control connection (BIND PORT=1)
	conn, err := net.DialTimeout("tcp", registryAddr, 5*time.Second)
	if err != nil {
		log.Fatalf("[FAIL] dial: %v", err)
	}

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		log.Fatalf("[FAIL] socks5 greet: %v", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		log.Fatalf("[FAIL] socks5 greet reply: %v", err)
	}
	if resp[1] != 0x00 {
		log.Fatalf("[FAIL] socks5 auth rejected: %v", resp)
	}

	addr := "control"
	bindReq := []byte{0x05, 0x02, 0x00, 0x03, byte(len(addr))}
	bindReq = append(bindReq, []byte(addr)...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, 1)
	bindReq = append(bindReq, portBuf...)

	if _, err := conn.Write(bindReq); err != nil {
		log.Fatalf("[FAIL] socks5 bind: %v", err)
	}
	bindResp := make([]byte, 10)
	if _, err := io.ReadFull(conn, bindResp); err != nil {
		log.Fatalf("[FAIL] bind reply: %v", err)
	}
	if bindResp[1] != 0x00 {
		log.Fatalf("[FAIL] bind rejected: status=%d", bindResp[1])
	}
	fmt.Println("[OK] Control connection established (BIND PORT=1)")
	defer conn.Close()

	// Step 2: Register request
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"cmd":            "register",
		"proto":          "socks5",
		"preferred_port": 19902,
	})
	if err := reverse.WriteFrame(conn, reverse.FrameData, reqJSON); err != nil {
		log.Fatalf("[FAIL] send register: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, payload, err := reverse.ReadFrame(conn)
	if err != nil {
		log.Fatalf("[FAIL] read register reply: %v", err)
	}
	conn.SetReadDeadline(time.Time{})

	var reply struct {
		Status  string `json:"status"`
		Address string `json:"address"`
		Port    int    `json:"port"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(payload, &reply); err != nil {
		log.Fatalf("[FAIL] parse reply: %v", err)
	}
	replyPretty, _ := json.MarshalIndent(reply, "", "  ")
	fmt.Printf("[OK] Register reply:\n%s\n", string(replyPretty))
	if reply.Status != "ok" {
		log.Fatalf("[FAIL] register rejected: %s", reply.Error)
	}

	dynAddr := reply.Address
	dynPort := reply.Port
	fmt.Printf("[INFO] Allocated address=%s, port=%d\n", dynAddr, dynPort)

	// Step 3: Data connection (BIND PORT=0)
	dataConn, err := net.DialTimeout("tcp", registryAddr, 5*time.Second)
	if err != nil {
		log.Fatalf("[FAIL] data dial: %v", err)
	}

	if _, err := dataConn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		log.Fatalf("[FAIL] data socks5 greet: %v", err)
	}
	dataResp := make([]byte, 2)
	if _, err := io.ReadFull(dataConn, dataResp); err != nil {
		log.Fatalf("[FAIL] data greet reply: %v", err)
	}
	if dataResp[1] != 0x00 {
		log.Fatalf("[FAIL] data auth rejected")
	}

	dataBindReq := []byte{0x05, 0x02, 0x00, 0x03, byte(len(dynAddr))}
	dataBindReq = append(dataBindReq, []byte(dynAddr)...)
	dataPortBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(dataPortBuf, 0)
	dataBindReq = append(dataBindReq, dataPortBuf...)

	if _, err := dataConn.Write(dataBindReq); err != nil {
		log.Fatalf("[FAIL] data bind: %v", err)
	}
	dataBindResp := make([]byte, 10)
	if _, err := io.ReadFull(dataConn, dataBindResp); err != nil {
		log.Fatalf("[FAIL] data bind reply: %v", err)
	}
	if dataBindResp[1] != 0x00 {
		log.Fatalf("[FAIL] data bind rejected: status=%d", dataBindResp[1])
	}
	fmt.Printf("[OK] Data connection established (BIND %s:0)\n", dynAddr)
	defer dataConn.Close()

	// Step 4: Run frame loop on data connection — handle all frame types
	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	// Control heartbeat
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				reverse.WriteFrame(conn, reverse.FrameHeartbeat, nil)
			}
		}
	}()

	// Data heartbeat — keep Registry's reader idle detector happy
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(25 * time.Second) // slightly faster than 30s server side
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				reverse.WriteFrame(dataConn, reverse.FrameHeartbeat, nil)
			}
		}
	}()

	// Data frame reader: unified handler for all frame types
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
			}

			dataConn.SetReadDeadline(time.Now().Add(120 * time.Second))
			frameType, payload, err := reverse.ReadFrame(dataConn)
			if err != nil {
				fmt.Printf("[DATA] read error: %v\n", err)
				return
			}

			switch frameType {
			case reverse.FrameHeartbeat:
				// Server heartbeat, ignore
			case reverse.FramePeng:
				// Registry sends PENG twice:
				// 1) After registration (HandleReverseConnection)
				// 2) During Match handshake (reverseHandshake sends PONG first, then reads PENG)
				// Always respond with PONG
				fmt.Println("[DATA] Received PENG, sending PONG")
				reverse.WriteFrame(dataConn, reverse.FramePong, nil)
			case reverse.FramePong:
				// Match() handshake: server sends PONG, expects PENG back
				fmt.Println("[DATA] Received PONG (from Match), sending PENG")
				reverse.WriteFrame(dataConn, reverse.FramePeng, nil)
			case reverse.FrameData:
				// Actual data from client → forward to target → send response back
				if len(payload) == 0 {
					continue
				}
				fmt.Printf("[DATA] Got %d bytes of data, forwarding to target\n", len(payload))

				targetConn, err := net.DialTimeout("tcp", "httpbin.org:80", 10*time.Second)
				if err != nil {
					fmt.Printf("[DATA] dial target fail: %v\n", err)
					continue
				}
				if _, err := targetConn.Write(payload); err != nil {
					targetConn.Close()
					fmt.Printf("[DATA] write to target fail: %v\n", err)
					continue
				}
				targetConn.SetReadDeadline(time.Now().Add(15 * time.Second))
				var respBuf []byte
				tmp := make([]byte, 4096)
				for {
					nn, rerr := targetConn.Read(tmp)
					if nn > 0 {
						respBuf = append(respBuf, tmp[:nn]...)
					}
					if rerr != nil {
						break
					}
				}
				targetConn.Close()

				if len(respBuf) > 0 {
					if err := reverse.WriteFrame(dataConn, reverse.FrameData, respBuf); err != nil {
						fmt.Printf("[DATA] write response fail: %v\n", err)
						return
					}
					fmt.Printf("[DATA] Response: %d bytes\n", len(respBuf))
				}
			default:
				fmt.Printf("[DATA] unknown frame type 0x%02x\n", frameType)
			}
		}
	}()

	// Verify listener
	time.Sleep(500 * time.Millisecond)
	testConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", dynPort), 2*time.Second)
	if err != nil {
		fmt.Printf("[WARN] Dynamic listener not reachable: %v\n", err)
	} else {
		testConn.Close()
		fmt.Println("[OK] Dynamic listener is accepting connections!")
	}

	fmt.Printf("\n[READY] Running... Ctrl+C to stop\n")
	fmt.Printf("  Test with: curl --socks5 127.0.0.1:%d http://httpbin.org/ip\n", dynPort)
	select {} // block forever
}
