package upgrade

import (
	"os"
	"testing"
)

func TestRequestStatusRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())

	if req := ReadRequest(); req != nil {
		t.Fatalf("empty dir returned request %+v", req)
	}
	if st := ReadStatus(); st.Status != StatusIdle {
		t.Fatalf("default status = %q, want %q", st.Status, StatusIdle)
	}

	if err := WriteRequest(Request{Version: "v0.8.0-665", PackageID: "abc"}); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	req := ReadRequest()
	if req == nil || req.Version != "v0.8.0-665" || req.PackageID != "abc" {
		t.Fatalf("ReadRequest = %+v", req)
	}
	if req.RequestedAt.IsZero() {
		t.Fatal("RequestedAt not filled")
	}

	if err := WriteStatus(Status{
		Status:    StatusFailedRolledBack,
		Current:   "v0.8.0-664",
		Requested: "v0.8.0-665",
		LastError: "boom",
	}); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	st := ReadStatus()
	if st.Status != StatusFailedRolledBack || st.Current != "v0.8.0-664" || st.LastError != "boom" {
		t.Fatalf("ReadStatus = %+v", st)
	}

	if err := WriteMarker(Marker{Old: "a", New: "b", Version: "v1"}); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if m := ReadMarker(); m == nil || m.Version != "v1" {
		t.Fatalf("ReadMarker = %+v", m)
	}

	if err := WriteWorkerVersion("v0.8.0-665"); err != nil {
		t.Fatalf("WriteWorkerVersion: %v", err)
	}
	if v := ReadWorkerVersion(); v != "v0.8.0-665" {
		t.Fatalf("ReadWorkerVersion = %q", v)
	}

	if err := ClearRequest(); err != nil {
		t.Fatalf("ClearRequest: %v", err)
	}
	if req := ReadRequest(); req != nil {
		t.Fatalf("request not cleared: %+v", req)
	}
	if err := ClearMarker(); err != nil {
		t.Fatalf("ClearMarker: %v", err)
	}
	if m := ReadMarker(); m != nil {
		t.Fatalf("marker not cleared: %+v", m)
	}
}

func TestCorruptStatusFallsBackToIdle(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("data/state", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatusPath, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if st := ReadStatus(); st.Status != StatusIdle {
		t.Fatalf("corrupt status = %q, want idle", st.Status)
	}
}
