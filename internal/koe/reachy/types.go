package reachy

import "encoding/json"

// ProtoVersion is the wire-protocol version this client speaks. major must match
// the bridge's hello.proto; a differing minor is fine (additive evolution).
const ProtoVersion = "1.0"

// Frame type identifiers (spec section 4). Closed set — a change is a spec PR
// across this repo + the bridge.
const (
	FrameRPCRequest = "motion_rpc_request"
	FrameRPCResult  = "motion_rpc_result"
	FrameEvent      = "motion_event"
	FrameStream     = "motion_stream"
)

// Methods (spec section 6).
const (
	MethodPing      = "system.ping"
	MethodHello     = "system.hello"
	MethodPlayMove  = "motion.play_move"
	MethodStopMoves = "motion.stop_moves"
	MethodLookAt    = "motion.look_at"
	MethodSetListen = "motion.set_listening"
	MethodWake      = "motion.wake"
	MethodSleep     = "motion.sleep"
	MethodGetStatus = "motion.get_status"
)

// Error codes (spec section 9). Closed set.
const (
	ErrCodeVersionMismatch   = "version_mismatch"
	ErrCodeHandshakeRequired = "handshake_required"
	ErrCodeUnknownMethod     = "unknown_method"
	ErrCodeInvalidArgument   = "invalid_argument"
	ErrCodeUnknownMove       = "unknown_move"
	ErrCodeDaemonUnavailable = "daemon_unavailable"
	ErrCodeMotorsUnavailable = "motors_unavailable"
	ErrCodeInternal          = "internal_error"
	ErrCodeTimeout           = "timeout"
)

// Event names (bridge -> Koe, spec section 8).
const (
	EventMoveStarted  = "move_started"
	EventMoveFinished = "move_finished"
	EventMoveFailed   = "move_failed"
	EventStatus       = "status"
)

// Stream names (Koe -> bridge, spec section 7).
const (
	StreamSpeechEnvelope = "speech_envelope"
	StreamFaceOffsets    = "face_offsets"
)

// RPCRequest is the motion_rpc_request payload (trimmed vs desktop_rpc — no
// session/agent/source).
type RPCRequest struct {
	RequestID string          `json:"request_id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	TimeoutMs int             `json:"timeout_ms,omitempty"`
	Ts        string          `json:"ts,omitempty"`
}

// RPCResult is the motion_rpc_result payload: ok=true carries Result, ok=false
// carries Error.
type RPCResult struct {
	RequestID string          `json:"request_id"`
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *RPCError       `json:"error,omitempty"`
}

// RPCError is the structured error inside a failed RPCResult.
type RPCError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retriable bool   `json:"retriable"`
}

// Error implements error so a returned RPCError can flow through Go error paths.
func (e *RPCError) Error() string { return e.Code + ": " + e.Message }

// Event is the motion_event payload (bridge -> Koe, no request_id).
type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
	Ts    string          `json:"ts,omitempty"`
}

// Stream is the motion_stream payload (Koe -> bridge, no id/ts/reply).
type Stream struct {
	Stream string `json:"stream"`
	Data   any    `json:"data"`
}

// Hello is the system.hello request params.
type Hello struct {
	Proto         string `json:"proto"`
	Client        string `json:"client"`
	ClientVersion string `json:"client_version"`
}

// HelloResult is the system.hello result (spec section 5).
type HelloResult struct {
	Proto         string   `json:"proto"`
	BridgeVersion string   `json:"bridge_version"`
	SdkVersion    string   `json:"sdk_version"`
	Moves         []string `json:"moves"`
	Capabilities  []string `json:"capabilities"`
}
