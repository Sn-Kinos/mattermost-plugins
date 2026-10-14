package limit

import (
	"context"
	"sync"
)

// Gate bounds work shared by otherwise independent callers.
type Gate struct {
	tokens chan struct{}
}

func New(capacity int) *Gate {
	if capacity < 1 {
		capacity = 1
	}
	return &Gate{tokens: make(chan struct{}, capacity)}
}

func (g *Gate) Acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case g.tokens <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-g.tokens
			return nil, err
		}
		return g.release(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *Gate) TryAcquire() (func(), bool) {
	select {
	case g.tokens <- struct{}{}:
		return g.release(), true
	default:
		return nil, false
	}
}

func (g *Gate) release() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			<-g.tokens
		})
	}
}
