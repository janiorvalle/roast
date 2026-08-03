package engine

import (
	"context"
	"fmt"
)

// FakeEngine returns a canned response and never starts a subprocess or
// contacts a model. With no response configured it returns a clean verdict.
type FakeEngine struct {
	Response           []byte
	responseConfigured bool
}

func NewFake(response []byte) *FakeEngine {
	return &FakeEngine{Response: append([]byte(nil), response...), responseConfigured: response != nil}
}

func (engine *FakeEngine) Review(ctx context.Context, request Request) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("[ROAST-ENGINE-FAKE] review canceled: %w", err)
	}
	if engine.responseConfigured || engine.Response != nil {
		if engine.Response == nil {
			return nil, fmt.Errorf("[ROAST-ENGINE-FAKE] no canned verdict is configured; pass --fake-verdict <path> with one verdict JSON object so a missing review cannot approve the change")
		}
		return append([]byte(nil), engine.Response...), nil
	}
	return nil, fmt.Errorf("[ROAST-ENGINE-FAKE] no canned verdict is configured; pass --fake-verdict <path> with one verdict JSON object so a missing review cannot approve the change")
}
