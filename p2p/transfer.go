package p2p

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"phaethon/reverse"
	"phaethon/util"
)

const chunkSize = 512 * 1024 // 512KB per chunk

// UpdateRequest is sent by low version to request binary sync.
type UpdateRequest struct {
	Cmd      string `json:"cmd"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	BuildTag string `json:"buildTag,omitempty"`
	Version  string `json:"version"`
}

// Manifest is sent by the holder with chunk hashes.
type Manifest struct {
	Cmd         string   `json:"cmd"`
	Platform    string   `json:"platform"`
	Arch        string   `json:"arch"`
	BuildTag    string   `json:"buildTag,omitempty"`
	Version     string   `json:"version"`
	TotalSize   int64    `json:"totalSize"`
	FileHash    string   `json:"fileHash"`
	ChunkSize   int      `json:"chunkSize"`
	TotalChunks int      `json:"totalChunks"`
	ChunkHashes []string `json:"chunkHashes"`
}

// ChunkReq requests a specific chunk from the sender.
type ChunkReq struct {
	Cmd        string `json:"cmd"`
	ChunkIndex int    `json:"chunkIndex"`
}

// ChunkHeader is the first frame of a chunk transmission (JSON).
type ChunkHeader struct {
	ChunkIndex  int `json:"chunkIndex"`
	FrameSeq    int `json:"frameSeq"`
	TotalFrames int `json:"totalFrames"`
}

// UpdateAck confirms receipt and verification of the update.
type UpdateAck struct {
	Cmd      string `json:"cmd"`
	Status   string `json:"status"` // "ok", "chunk_mismatch", "error"
	FileHash string `json:"fileHash,omitempty"`
	Error    string `json:"error,omitempty"`
}

// generateManifest reads a binary file and generates a manifest with chunk hashes.
func generateManifest(filePath, version, platform, arch, buildTag string) (*Manifest, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", filePath, err)
	}

	fileHash := sha256.Sum256(data)
	totalChunks := (len(data) + chunkSize - 1) / chunkSize
	chunkHashes := make([]string, totalChunks)

	for i := 0; i < totalChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}
		hash := sha256.Sum256(data[start:end])
		chunkHashes[i] = "sha256:" + hex.EncodeToString(hash[:])
	}

	return &Manifest{
		Cmd:         "manifest",
		Platform:    platform,
		Arch:        arch,
		BuildTag:    buildTag,
		Version:     version,
		TotalSize:   int64(len(data)),
		FileHash:    "sha256:" + hex.EncodeToString(fileHash[:]),
		ChunkSize:   chunkSize,
		TotalChunks: totalChunks,
		ChunkHashes: chunkHashes,
	}, nil
}

// sendChunk reads and sends a specific chunk from a file to the peer.
func sendChunk(peer *Peer, filePath string, chunkIndex int) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file %s: %w", filePath, err)
	}
	defer f.Close()

	if _, err := f.Seek(int64(chunkIndex)*int64(chunkSize), io.SeekStart); err != nil {
		return fmt.Errorf("seek to chunk: %w", err)
	}

	chunkData := make([]byte, chunkSize)
	n, err := io.ReadFull(f, chunkData)
	if err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("read chunk: %w", err)
	}
	chunkData = chunkData[:n]

	totalFrames := (len(chunkData) + reverse.MaxPayload - 1) / reverse.MaxPayload
	if totalFrames == 0 {
		totalFrames = 1
	}

	header := ChunkHeader{
		ChunkIndex:  chunkIndex,
		FrameSeq:    0,
		TotalFrames: totalFrames,
	}
	headerData, _ := json.Marshal(header)
	enqueueWrite(peer, reverse.FrameData, headerData)

	for i := 0; i < totalFrames; i++ {
		start := i * reverse.MaxPayload
		end := start + reverse.MaxPayload
		if end > len(chunkData) {
			end = len(chunkData)
		}
		enqueueWrite(peer, reverse.FrameData, chunkData[start:end])
	}

	util.LogDebug("[P2P] sent chunk %d (%d bytes, %d frames) to %s", chunkIndex, len(chunkData), totalFrames, peer.ID)
	return nil
}

// handleUpdateRequest processes an update request from a peer.
func (m *P2PManager) handleUpdateRequest(peer *Peer, payload []byte) {
	var req UpdateRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		util.LogDebug("[P2P] invalid update_request from %s: %v", peer.ID, err)
		return
	}

	util.LogInfo("[P2P] update request from %s for %s/%s/%s version=%s",
		peer.ID, req.Platform, req.Arch, req.BuildTag, req.Version)

	// Look up in cache
	if !m.cache.HasEntry(req.Platform, req.Arch, req.BuildTag, req.Version) {
		util.LogInfo("[P2P] don't have %s/%s/%s/%s in cache", peer.ID, req.Platform, req.Arch, req.BuildTag, req.Version)
		ack := UpdateAck{Cmd: "update_ack", Status: "error", Error: "not in cache"}
		data, _ := json.Marshal(ack)
		enqueueWrite(peer, reverse.FrameData, data)
		return
	}

	cachePath, _ := m.cache.FilePath(req.Platform, req.Arch, req.BuildTag, req.Version)

	manifest, err := generateManifest(cachePath, req.Version, req.Platform, req.Arch, req.BuildTag)
	if err != nil {
		util.LogError("[P2P] failed to generate manifest: %v", err)
		ack := UpdateAck{Cmd: "update_ack", Status: "error", Error: err.Error()}
		data, _ := json.Marshal(ack)
		enqueueWrite(peer, reverse.FrameData, data)
		return
	}

	// Remember which file to serve chunks from
	peer.serveFilePath = cachePath

	manifestData, _ := json.Marshal(manifest)
	enqueueWrite(peer, reverse.FrameData, manifestData)

	util.LogInfo("[P2P] sent manifest to %s: %d chunks, %d bytes", peer.ID, manifest.TotalChunks, manifest.TotalSize)
}

// handleChunkReq processes a chunk request from a peer.
func (m *P2PManager) handleChunkReq(peer *Peer, payload []byte) {
	var req ChunkReq
	if err := json.Unmarshal(payload, &req); err != nil {
		util.LogDebug("[P2P] invalid chunk_req from %s: %v", peer.ID, err)
		return
	}

	if peer.serveFilePath == "" {
		util.LogError("[P2P] chunk_req from %s but no file path set", peer.ID)
		return
	}

	if err := sendChunk(peer, peer.serveFilePath, req.ChunkIndex); err != nil {
		util.LogError("[P2P] failed to send chunk %d to %s: %v", req.ChunkIndex, peer.ID, err)
	}
}

// handleManifest processes a manifest from a peer (we are the receiver).
func (m *P2PManager) handleManifest(peer *Peer, payload []byte) {
	var manifest Manifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		util.LogDebug("[P2P] invalid manifest from %s: %v", peer.ID, err)
		return
	}

	util.LogInfo("[P2P] received manifest from %s: %s/%s/%s version=%s, %d chunks, %d bytes",
		peer.ID, manifest.Platform, manifest.Arch, manifest.BuildTag, manifest.Version,
		manifest.TotalChunks, manifest.TotalSize)

	peer.Status = "updating"

	// Request chunks one by one, collecting into a buffer
	hasher := sha256.New()
	var buf bytes.Buffer

	for i := 0; i < manifest.TotalChunks; i++ {
		req := ChunkReq{Cmd: "chunk_req", ChunkIndex: i}
		reqData, _ := json.Marshal(req)
		enqueueWrite(peer, reverse.FrameData, reqData)

		chunkData, err := m.receiveChunk(peer, i)
		if err != nil {
			util.LogError("[P2P] failed to receive chunk %d: %v", i, err)
			peer.Status = "failed"
			return
		}

		// Verify chunk hash
		chunkHash := sha256.Sum256(chunkData)
		expectedHash := manifest.ChunkHashes[i]
		actualHash := "sha256:" + hex.EncodeToString(chunkHash[:])
		if actualHash != expectedHash {
			util.LogError("[P2P] chunk %d hash mismatch: expected %s, got %s", i, expectedHash, actualHash)
			ack := UpdateAck{Cmd: "update_ack", Status: "chunk_mismatch"}
			data, _ := json.Marshal(ack)
			enqueueWrite(peer, reverse.FrameData, data)
			peer.Status = "failed"
			return
		}

		hasher.Write(chunkData)
		buf.Write(chunkData)

		util.LogDebug("[P2P] received and verified chunk %d/%d", i+1, manifest.TotalChunks)
	}

	// Verify overall file hash
	fileHash := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if fileHash != manifest.FileHash {
		util.LogError("[P2P] file hash mismatch: expected %s, got %s", manifest.FileHash, fileHash)
		ack := UpdateAck{Cmd: "update_ack", Status: "error", Error: "file hash mismatch"}
		data, _ := json.Marshal(ack)
		enqueueWrite(peer, reverse.FrameData, data)
		peer.Status = "failed"
		return
	}

	// Store into cache
	cachePath, err := m.cache.StoreFromReader(manifest.Platform, manifest.Arch, manifest.BuildTag, manifest.Version, &buf)
	if err != nil {
		util.LogError("[P2P] failed to store in cache: %v", err)
		peer.Status = "failed"
		return
	}

	// Send success ack
	ack := UpdateAck{Cmd: "update_ack", Status: "ok", FileHash: fileHash}
	ackData, _ := json.Marshal(ack)
	enqueueWrite(peer, reverse.FrameData, ackData)

	util.LogInfo("[P2P] sync complete from %s: stored %s", peer.ID, cachePath)
	peer.Status = "upToDate"

	// Check if this binary matches our platform and is newer → self-update
	m.checkSelfUpdate(manifest.Platform, manifest.Arch, manifest.BuildTag, manifest.Version, cachePath)
}

// checkSelfUpdate checks if the received binary should trigger a self-update.
func (m *P2PManager) checkSelfUpdate(platform, arch, buildTag, version, cachePath string) {
	if platform != m.platform || arch != m.arch || buildTag != m.buildTag {
		util.LogDebug("[P2P] cached binary %s/%s/%s doesn't match self (%s/%s/%s), skip self-update",
			platform, arch, buildTag, m.platform, m.arch, m.buildTag)
		return
	}

	if compareVersions(version, m.version) <= 0 {
		util.LogDebug("[P2P] cached version %s is not newer than running %s, skip self-update", version, m.version)
		return
	}

	util.LogInfo("[P2P] self-update: %s → %s from %s", m.version, version, cachePath)
	performSelfUpdate(cachePath, version, platform, arch, buildTag)
}

// receiveChunk receives a chunk from the peer via the chunk channel.
// Frames are routed to the channel by runSession when receivingChunks is true.
func (m *P2PManager) receiveChunk(peer *Peer, expectedIndex int) ([]byte, error) {
	// Read chunk header from channel (skip heartbeats)
	var frame chunkFrame
	for {
		select {
		case frame = <-peer.chunkCh:
		case <-peer.stopCh:
			return nil, fmt.Errorf("peer stopped")
		case <-time.After(60 * time.Second):
			return nil, fmt.Errorf("timeout waiting for chunk header")
		}

		if frame.frameType == reverse.FrameHeartbeat {
			continue
		}
		if frame.frameType != reverse.FrameData {
			return nil, fmt.Errorf("expected FrameData for chunk header, got 0x%02x", frame.frameType)
		}
		break
	}

	var header ChunkHeader
	if err := json.Unmarshal(frame.payload, &header); err != nil {
		return nil, fmt.Errorf("parse chunk header: %w", err)
	}
	if header.ChunkIndex != expectedIndex {
		return nil, fmt.Errorf("chunk index mismatch: expected %d, got %d", expectedIndex, header.ChunkIndex)
	}

	data := make([]byte, 0, header.TotalFrames*reverse.MaxPayload)
	for i := 0; i < header.TotalFrames; i++ {
		select {
		case frame = <-peer.chunkCh:
		case <-peer.stopCh:
			return nil, fmt.Errorf("peer stopped")
		case <-time.After(60 * time.Second):
			return nil, fmt.Errorf("timeout waiting for chunk frame %d", i)
		}

		if frame.frameType == reverse.FrameHeartbeat {
			i--
			continue
		}
		if frame.frameType != reverse.FrameData {
			return nil, fmt.Errorf("expected FrameData for chunk frame %d, got 0x%02x", i, frame.frameType)
		}
		data = append(data, frame.payload...)
	}

	return data, nil
}

// requestUpdate sends an update request to a peer for a specific cache entry.
func (m *P2PManager) requestUpdate(peer *Peer, entry CacheEntry) {
	req := UpdateRequest{
		Cmd:      "update_request",
		Platform: entry.Platform,
		Arch:     entry.Arch,
		BuildTag: entry.BuildTag,
		Version:  entry.Version,
	}
	data, _ := json.Marshal(req)
	enqueueWrite(peer, reverse.FrameData, data)
	util.LogInfo("[P2P] sent update request to %s for %s/%s/%s/%s",
		peer.ID, entry.Platform, entry.Arch, entry.BuildTag, entry.Version)
}
