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

func TestFakeTreatsAnEmptyResponseAsNoVerdict(t *testing.T) {
	if _, err := NewFake([]byte{}).Review(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "no canned verdict") {
		t.Fatalf("error = %v", err)
	}
	fake := NewFakeSequence([][]byte{[]byte("first"), {}})
	if _, err := fake.Review(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.Review(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "review call 2 has an empty canned verdict") {
		t.Fatalf("error = %v", err)
	}
}

func TestFakeSequenceServesOneResponsePerCallInOrder(t *testing.T) {
	fake := NewFakeSequence([][]byte{[]byte("first"), []byte("second")})
	for _, want := range []string{"first", "second"} {
		got, err := fake.Review(context.Background(), Request{Prompt: "prompt"})
		if err != nil || string(got) != want {
			t.Fatalf("response = %q, err = %v, want %q", got, err, want)
		}
	}
	_, err := fake.Review(context.Background(), Request{Prompt: "prompt"})
	if err == nil || !strings.Contains(err.Error(), "review call 3 has no canned verdict") || !strings.Contains(err.Error(), "holds 2 verdict(s)") {
		t.Fatalf("error = %v", err)
	}
}
