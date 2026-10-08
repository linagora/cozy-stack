package sharings

import (
	"context"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

func TestForwardEventsStopsWhenClientIsGone(t *testing.T) {
	ds := &realtime.Subscriber{Channel: make(realtime.EventsChan, 1)}
	ch := make(chan *wsResponse)
	ctx, cancel := context.WithCancel(context.Background())

	ds.Channel <- &realtime.Event{
		Verb: realtime.EventUpdate,
		Doc:  &realtime.JSONDoc{Type: consts.Files, M: map[string]interface{}{"_id": "foo"}},
	}

	done := make(chan struct{})
	go func() {
		forwardEvents(ctx, ds, ch, func(*realtime.Event) bool { return true })
		close(done)
	}()

	for len(ds.Channel) > 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forwardEvents kept running after the websocket writer stopped")
	}
}
