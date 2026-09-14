package engine

import (
	"context"
	"fmt"
)

// FakeEngine returns canned responses, one per review call in order, and
// never starts a subprocess or contacts a model. A chunked review makes one
// call per chunk, so it needs one canned verdict per chunk.
type FakeEngine struct {
	responses [][]byte
	served    int
}

// NewFake serves one canned response. An empty response means none is configured.
func NewFake(response []byte) *FakeEngine {
	if len(response) == 0 {
		return NewFakeSequence(nil)
	}
	return NewFakeSequence([][]byte{response})
}

// NewFakeSequence serves the responses in order, one per review call.
func NewFakeSequence(responses [][]byte) *FakeEngine {
	copied := make([][]byte, 0, len(responses))
	for _, response := range responses {
		copied = append(copied, append([]byte(nil), response...))
	}
	return &FakeEngine{responses: copied}
}

func (engine *FakeEngine) Review(ctx context.Context, request Request) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("[ROAST-ENGINE-FAKE] review canceled: %w", err)
	}
	if engine.served >= len(engine.responses) {
		if len(engine.responses) == 0 {
			return nil, fmt.Errorf("[ROAST-ENGINE-FAKE] no canned verdict is configured; pass --fake-verdict <path> with one verdict JSON object so a missing review cannot approve the change")
		}
		return nil, fmt.Errorf("[ROAST-ENGINE-FAKE] review call %d has no canned verdict; the fake verdict file holds %d verdict(s), so pass a JSON array with one verdict per chunk", engine.served+1, len(engine.responses))
	}
	response := engine.responses[engine.served]
	engine.served++
	if len(response) == 0 {
		return nil, fmt.Errorf("[ROAST-ENGINE-FAKE] review call %d has an empty canned verdict; pass --fake-verdict <path> with one verdict JSON object per chunk so a missing review cannot approve the change", engine.served)
	}
	return append([]byte(nil), response...), nil
}
