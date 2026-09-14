package subagent

import (
	"context"
	"sync"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// gatedModel limits concurrent provider requests, not whole agent lifetimes.
// A run may synchronously spawn another run while it is executing a tool; holding
// a permit for the whole lifetime would deadlock that child when the gate is full.
type gatedModel struct {
	base model.Model
	gate *gate
}

func (m *gatedModel) Stream(ctx context.Context, msgs []message.Message, tools []model.ToolSpec) (model.ModelStream, error) {
	if err := m.gate.acquire(ctx); err != nil {
		return nil, err
	}
	stream, err := m.base.Stream(ctx, msgs, tools)
	if err != nil {
		m.gate.release()
		return nil, err
	}
	return &gatedStream{ModelStream: stream, release: m.gate.release}, nil
}

type gatedStream struct {
	model.ModelStream
	once    sync.Once
	release func()
}

func (s *gatedStream) Close() {
	defer s.once.Do(s.release)
	s.ModelStream.Close()
}
