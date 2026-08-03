package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/janiorvalle/roast/internal/verdict"
)

func TestFakeReturnsConfiguredResponseWithoutChangingIt(t *testing.T) {
	response := []byte(`{"overall":"well_done","findings":[],"provenance":{"target":"HEAD","branch":"main","tree":"abc","engine":"test","context":"snapshot"}}`)
	got, err := NewFake(response).Review(context.Background(), Request{Prompt: "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(response) {
		t.Fatalf("response = %q, want %q", got, response)
	}
}

func TestFakeRequiresCannedResponse(t *testing.T) {
	if _, err := NewFake(nil).Review(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "no canned verdict") {
		t.Fatalf("error = %v", err)
	}
}

func TestFakeReturnsConfiguredResponseVerbatim(t *testing.T) {
	provenance := verdict.Provenance{Target: "HEAD", Branch: "main", Tree: "abc", Engine: "fake/canned", Context: "snapshot"}
	response := []byte(`{"overall":"well_done","findings":[],"provenance":{"target":"HEAD","branch":"main","tree":"abc","engine":"fake/canned","context":"snapshot"}}`)
	got, err := NewFake(response).Review(context.Background(), Request{Prompt: "prompt", Provenance: provenance})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := verdict.Decode(got)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Overall != verdict.OverallWellDone || decoded.Provenance != provenance {
		t.Fatalf("decoded = %#v", decoded)
	}
}

func TestFakeHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewFake(nil).Review(ctx, Request{}); err == nil || !strings.Contains(err.Error(), "ROAST-ENGINE-FAKE") {
		t.Fatalf("error = %v", err)
	}
}

func TestFakePreservesExplicitEmptyResponse(t *testing.T) {
	if _, err := NewFake([]byte{}).Review(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "no canned verdict") {
		t.Fatalf("error = %v", err)
	}
}
