//go:build darwin && cgo

package koe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func enableNativeFloorForTest(t *testing.T) {
	t.Helper()
	t.Setenv("KOE_VPIO_BARGE_IN", "1")
	t.Setenv("KOE_NATIVE_FLOOR", "1")
	t.Setenv("KOE_INTERRUPT_RESPONSE", "0")
}

func TestNativeFloorControllerDefaultsMalformedJudgeToResume(t *testing.T) {
	floor := newNativeFloorController()
	if !floor.begin("source-1") || !floor.noteUserCommit(7) || !floor.bindJudge("judge-1", 7) {
		t.Fatal("failed to establish native floor turn")
	}
	if got := floor.finishResponse("judge-1"); got != floorDecisionResume {
		t.Fatalf("malformed judge decision = %v, want resume", got)
	}
	if floor.holdsPlayback() {
		t.Fatal("finished judge must release floor state")
	}
}

func TestNativeFloorControllerDeduplicatesDecision(t *testing.T) {
	floor := newNativeFloorController()
	floor.begin("source-1")
	floor.noteUserCommit(3)
	floor.bindJudge("judge-1", 3)

	claim := floor.claim("judge-1", "call-1", "accept_turn")
	if !claim.handled || claim.decision != floorDecisionAccept || claim.turnID != 3 {
		t.Fatalf("first claim = %+v, want accepted turn 3", claim)
	}
	duplicate := floor.claim("judge-1", "call-1", "accept_turn")
	if !duplicate.handled || !duplicate.duplicate {
		t.Fatalf("duplicate claim = %+v, want handled duplicate", duplicate)
	}
	if got := floor.finishResponse("judge-1"); got != floorDecisionNone {
		t.Fatalf("already-applied decision finished as %v, want none", got)
	}
}

func TestNativeFloorResponseHasOnlyNarrowRequiredTools(t *testing.T) {
	payload := responseCreatePayload(responseCreateRequest{
		instructions: nativeFloorInstructions,
		purpose:      responsePurposeFloor,
		turnID:       4,
		toolMode:     responseToolsFloor,
	})
	raw, _ := json.Marshal(payload)
	var decoded struct {
		Response struct {
			Tools             []ToolDef `json:"tools"`
			ToolChoice        string    `json:"tool_choice"`
			ParallelToolCalls bool      `json:"parallel_tool_calls"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Response.ToolChoice != "required" || decoded.Response.ParallelToolCalls {
		t.Fatalf("floor response policy = choice %q parallel %v", decoded.Response.ToolChoice, decoded.Response.ParallelToolCalls)
	}
	if len(decoded.Response.Tools) != 2 || decoded.Response.Tools[0].Name != "resume_playback" || decoded.Response.Tools[1].Name != "accept_turn" {
		t.Fatalf("floor tools = %+v, want only resume_playback and accept_turn", decoded.Response.Tools)
	}
}

func TestNativeFloorAcceptDiscardsPlaybackAndQueuesRawTurnResponse(t *testing.T) {
	enableNativeFloorForTest(t)
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatal(err)
	}
	state := NewCallState("burst-floor", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, audio, cap.send)
	h.fullDuplexAEC = true

	h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"source-1"}}`))
	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.started"}`))
	audio.Play(make([]int16, audioFrameSize))
	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.speech_started"}`))
	if !audio.PlaybackPaused() || len(audio.playBuf) != 1 {
		t.Fatalf("talk-over pause = paused %v queued %d, want true/1", audio.PlaybackPaused(), len(audio.playBuf))
	}
	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.committed"}`))

	judgeReq := <-h.loopRespReq
	if judgeReq.purpose != responsePurposeFloor || judgeReq.turnID != 1 {
		t.Fatalf("judge request = %+v, want floor turn 1", judgeReq)
	}
	// The source response can finish while its exact PCM and any result lease remain
	// held for the narrow decision.
	h.handleEvent(context.Background(), []byte(`{"type":"response.done","response":{"id":"source-1","status":"completed"}}`))
	if !audio.PlaybackPaused() || len(audio.playBuf) != 1 {
		t.Fatal("source response.done must not discard paused playback")
	}

	h.setPendingResponse(judgeReq)
	h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"judge-1"}}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.function_call_arguments.done","response_id":"judge-1","call_id":"floor-call","name":"accept_turn","arguments":"{}"}`))
	if audio.PlaybackPaused() || audio.dropCapture() || len(audio.playBuf) != 0 {
		t.Fatalf("accept state = paused %v speaking %v queued %d, want false/false/0", audio.PlaybackPaused(), audio.dropCapture(), len(audio.playBuf))
	}
	accepted := <-h.loopRespReq
	if accepted.purpose != responsePurposeUser || accepted.turnID != 1 || accepted.toolMode != responseToolsEnabled {
		t.Fatalf("accepted response = %+v, want normal tools-enabled turn 1", accepted)
	}
	if cap.countType("output_audio_buffer.clear") != 1 || !cap.sentContains("accepted") {
		t.Fatalf("floor accept output frames = %v", cap.types())
	}
}

func TestNativeFloorResumeRetainsExactPlaybackWithoutReply(t *testing.T) {
	enableNativeFloorForTest(t)
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatal(err)
	}
	state := NewCallState("burst-floor", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, audio, cap.send)
	h.fullDuplexAEC = true

	h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"source-1"}}`))
	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.started"}`))
	wantPCM := make([]int16, audioFrameSize)
	wantPCM[0] = 321
	audio.Play(wantPCM)
	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.speech_started"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.committed"}`))
	judgeReq := <-h.loopRespReq
	h.handleEvent(context.Background(), []byte(`{"type":"response.done","response":{"id":"source-1","status":"completed"}}`))
	h.setPendingResponse(judgeReq)
	h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"judge-1"}}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.function_call_arguments.done","response_id":"judge-1","call_id":"floor-call","name":"resume_playback","arguments":"{}"}`))

	if audio.PlaybackPaused() || !audio.dropCapture() || len(audio.playBuf) != 1 {
		t.Fatalf("resume state = paused %v speaking %v queued %d, want false/true/1", audio.PlaybackPaused(), audio.dropCapture(), len(audio.playBuf))
	}
	if got := (<-audio.playBuf)[0]; got != wantPCM[0] {
		t.Fatalf("resumed PCM first sample = %d, want exact retained %d", got, wantPCM[0])
	}
	if len(h.loopRespReq) != 0 || cap.countType("output_audio_buffer.clear") != 0 || !cap.sentContains("resumed") {
		t.Fatalf("resume must not clear playback or queue a reply; frames=%v queued=%d", cap.types(), len(h.loopRespReq))
	}
}

func TestSessionConfigNativeFloorOwnsResponseCreation(t *testing.T) {
	enableNativeFloorForTest(t)
	raw, _ := json.Marshal(sessionConfig("persona", "marin", true))
	s := string(raw)
	for _, want := range []string{
		`"type":"server_vad"`,
		`"create_response":false`,
		`"interrupt_response":false`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("native floor session config missing %s in %s", want, s)
		}
	}
}

func TestNativeFloorJudgeTimeoutResumesInsteadOfStrandingAudio(t *testing.T) {
	enableNativeFloorForTest(t)
	t.Setenv("KOE_NATIVE_FLOOR_RESOLVE_MS", "20")
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatal(err)
	}
	state := NewCallState("burst-floor", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, audio, cap.send)
	h.fullDuplexAEC = true

	h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"source-1"}}`))
	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.started"}`))
	audio.Play(make([]int16, audioFrameSize))
	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.speech_started"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.committed"}`))
	judgeReq := <-h.loopRespReq
	h.handleEvent(context.Background(), []byte(`{"type":"response.done","response":{"id":"source-1","status":"completed"}}`))
	h.setPendingResponse(judgeReq)
	h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"judge-timeout"}}`))

	deadline := time.Now().Add(time.Second)
	for audio.PlaybackPaused() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if audio.PlaybackPaused() || !audio.dropCapture() || len(audio.playBuf) != 1 {
		t.Fatalf("timeout state = paused %v speaking %v queued %d, want fail-safe resumed", audio.PlaybackPaused(), audio.dropCapture(), len(audio.playBuf))
	}
	if cap.countType("response.cancel") != 1 {
		t.Fatalf("timeout frames = %v, want judge response.cancel", cap.types())
	}
}
