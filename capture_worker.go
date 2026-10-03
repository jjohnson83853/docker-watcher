package watcher

import (
	"context"
	"sync"
)

type CaptureFunc func(context.Context) (string, error)
type CaptureWorker struct {
	capture        CaptureFunc
	cancelCallback func()
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	mu             sync.Mutex
	result         string
	err            error
	started        bool
	canceled       bool
}

func NewCaptureWorker(capture CaptureFunc) *CaptureWorker {
	return NewCaptureWorkerWithCancel(capture, nil)
}
func NewCaptureWorkerWithCancel(capture CaptureFunc, cancelCallback func()) *CaptureWorker {
	return newCaptureWorker(context.Background(), capture, cancelCallback)
}
func newCaptureWorker(parent context.Context, capture CaptureFunc, cancelCallback func()) *CaptureWorker {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &CaptureWorker{capture: capture, cancelCallback: cancelCallback, ctx: ctx, cancel: cancel, done: make(chan struct{}), result: "unavailable"}
}
func (w *CaptureWorker) Start() {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.mu.Unlock()
	go func() {
		result, err := w.capture(w.ctx)
		w.mu.Lock()
		if w.canceled {
			w.result = "unavailable"
		} else if result != "" {
			w.result = result
		}
		w.err = err
		close(w.done)
		w.mu.Unlock()
	}()
}
func (w *CaptureWorker) Cancel() {
	w.mu.Lock()
	select {
	case <-w.done:
		w.mu.Unlock()
		return
	default:
	}
	w.canceled = true
	callback := w.cancelCallback
	w.mu.Unlock()
	w.cancel()
	if callback != nil {
		callback()
	}
}
func (w *CaptureWorker) Done() <-chan struct{} { return w.done }
func (w *CaptureWorker) Result() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.canceled {
		return "unavailable"
	}
	return w.result
}
func (w *CaptureWorker) IsAlive() bool {
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}
func (w *CaptureWorker) Error() error { w.mu.Lock(); defer w.mu.Unlock(); return w.err }
