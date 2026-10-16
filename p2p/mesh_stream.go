package p2p

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	"phaethon/util"
)

// MeshStreamHandler handles mesh protocol streams.
type MeshStreamHandler struct {
	p2pMgr *P2PManager

	mu      sync.RWMutex
	streams map[peer.ID]network.Stream // peer -> active stream
}

// NewMeshStreamHandler creates a new MeshStreamHandler.
func NewMeshStreamHandler(p2pMgr *P2PManager) *MeshStreamHandler {
	return &MeshStreamHandler{
		p2pMgr:  p2pMgr,
		streams: make(map[peer.ID]network.Stream),
	}
}

// HandleStream handles an incoming mesh stream.
func (h *MeshStreamHandler) HandleStream(stream network.Stream) {
	defer stream.Close()

	peerID := stream.Conn().RemotePeer()
	util.LogInfo("P2P", "New mesh stream from peer: %s", peerID)

	// Read mesh packets from the stream
	for {
		// Read length prefix (4 bytes)
		var length uint32
		if err := binary.Read(stream, binary.BigEndian, &length); err != nil {
			if err == io.EOF {
				util.LogInfo("P2P", "Stream closed by peer: %s", peerID)
			} else {
				util.LogError("P2P", "Failed to read from stream (peer=%s): %v", peerID, err)
			}
			return
		}

		// Read packet data
		data := make([]byte, length)
		if _, err := io.ReadFull(stream, data); err != nil {
			util.LogError("P2P", "Failed to read packet data (peer=%s): %v", peerID, err)
			return
		}

		// Process mesh packet
		// TODO: Integrate with MeshManager to handle the packet
		util.LogDebug("P2P", "Received mesh packet from %s: %d bytes", peerID, length)
	}
}

// SendPacket sends a mesh packet to a peer through a specific proxy.
func (h *MeshStreamHandler) SendPacket(ctx context.Context, proxyName string, peerID peer.ID, data []byte) error {
	// Get or create stream for this peer
	stream, err := h.getOrCreateStream(ctx, proxyName, peerID)
	if err != nil {
		return fmt.Errorf("get stream: %w", err)
	}

	// Write length prefix
	if err := binary.Write(stream, binary.BigEndian, uint32(len(data))); err != nil {
		return fmt.Errorf("write length: %w", err)
	}

	// Write packet data
	if _, err := stream.Write(data); err != nil {
		return fmt.Errorf("write data: %w", err)
	}

	return nil
}

// getOrCreateStream gets or creates a stream to a peer.
func (h *MeshStreamHandler) getOrCreateStream(ctx context.Context, proxyName string, peerID peer.ID) (network.Stream, error) {
	h.mu.RLock()
	stream, ok := h.streams[peerID]
	h.mu.RUnlock()

	if ok && stream != nil {
		return stream, nil
	}

	// Create new stream
	h.mu.Lock()
	defer h.mu.Unlock()

	// Double-check after acquiring write lock
	if stream, ok := h.streams[peerID]; ok && stream != nil {
		return stream, nil
	}

	// Create new stream through the specified proxy
	stream, err := h.p2pMgr.NewStream(ctx, proxyName, peerID)
	if err != nil {
		return nil, fmt.Errorf("create stream: %w", err)
	}

	h.streams[peerID] = stream
	util.LogInfo("P2P", "Created stream to peer %s via proxy %s", peerID, proxyName)

	return stream, nil
}

// RemoveStream removes a stream for a peer.
func (h *MeshStreamHandler) RemoveStream(peerID peer.ID) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if stream, ok := h.streams[peerID]; ok {
		stream.Close()
		delete(h.streams, peerID)
		util.LogInfo("P2P", "Removed stream for peer: %s", peerID)
	}
}
