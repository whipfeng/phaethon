package main

import (
	"strings"
	"testing"
)

func TestHandoffStateRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())

	req := handoffRequest{
		Token:         "tok-1",
		OldPID:        123,
		OldVersion:    "v0.8.0-697-gold",
		TargetVersion: "v0.8.0-698-gnew",
		Status:        handoffRequestStarting,
	}
	if err := writeHandoffJSON(handoffRequestPath, req); err != nil {
		t.Fatal(err)
	}
	got := readHandoffRequest()
	if got == nil || got.Token != req.Token || got.Status != handoffRequestStarting {
		t.Fatalf("request round trip = %+v", got)
	}

	req.Status = handoffRequestAbort
	if err := writeHandoffJSON(handoffRequestPath, req); err != nil {
		t.Fatal(err)
	}
	if !handoffAbortRequested("tok-1") {
		t.Fatal("abort not observed")
	}
	if handoffAbortRequested("other") {
		t.Fatal("abort observed for the wrong token")
	}

	res := handoffResult{Token: "tok-1", NewPID: 456, NewVersion: req.TargetVersion, Status: handoffResultReady}
	if err := writeHandoffJSON(handoffResultPath, res); err != nil {
		t.Fatal(err)
	}
	if got := readHandoffResult(); got == nil || got.Status != handoffResultReady || got.NewPID != 456 {
		t.Fatalf("result round trip = %+v", got)
	}

	clearHandoffFiles()
	if readHandoffRequest() != nil || readHandoffResult() != nil {
		t.Fatal("handoff files not cleared")
	}
}

func TestPublishHandoffResultRequiresMatchingToken(t *testing.T) {
	t.Chdir(t.TempDir())
	req := handoffRequest{Token: "tok-match", Status: handoffRequestStarting}
	if err := writeHandoffJSON(handoffRequestPath, req); err != nil {
		t.Fatal(err)
	}

	publishHandoffResult("tok-other", handoffResultReady, "v698", "")
	if res := readHandoffResult(); res != nil {
		t.Fatalf("result written for wrong token: %+v", res)
	}

	publishHandoffResult("tok-match", handoffResultReady, "v698", "")
	res := readHandoffResult()
	if res == nil || res.Status != handoffResultReady || res.NewVersion != "v698" {
		t.Fatalf("result = %+v", res)
	}
}

func TestHandoffEnvStripsWorkerAndOldHandoff(t *testing.T) {
	t.Setenv("PHAETHON_WORKER", "1")
	t.Setenv(handoffEnvToken, "old-token")
	t.Setenv(handoffEnvTarget, "old-target")

	env := handoffEnv("new-token", "new-target")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "PHAETHON_WORKER=") {
		t.Fatal("PHAETHON_WORKER leaked into handoff env")
	}
	if strings.Contains(joined, handoffEnvToken+"=old-token") || strings.Contains(joined, handoffEnvTarget+"=old-target") {
		t.Fatal("old handoff variables leaked")
	}
	if !strings.Contains(joined, handoffEnvToken+"=new-token") || !strings.Contains(joined, handoffEnvTarget+"=new-target") {
		t.Fatal("new handoff variables missing")
	}
}
