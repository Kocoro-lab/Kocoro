package koe

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// interruptOutput is the synchronous release path (timer paths are async) —
// exercise the restore rule through it.
func TestUserMicRestoreOnReleaseRespectsInFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(srv.URL), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatal(err)
	}
	h := newEventHandler(disp, state, audio, func(any) error { return nil })

	audio.SetUserMicOff(true)
	state.SetInFlightForAgent("check mail", "")
	h.interruptOutput()
	if !audio.UserMicOff() {
		t.Fatal("mic restored while a task is still in flight")
	}

	state.ClearInFlightForAgent("")
	h.interruptOutput()
	if audio.UserMicOff() {
		t.Fatal("mic not restored after the last task drained")
	}
}

func TestMaybeRestoreUserMicNoAudioIsSafe(t *testing.T) {
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient("http://127.0.0.1:1"), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, nil, func(any) error { return nil })
	h.maybeRestoreUserMic() // must not panic with nil audio
}
