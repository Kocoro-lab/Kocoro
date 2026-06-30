//go:build !darwin || !cgo

package koe

import "errors"

// StartVPIO is available only on macOS with cgo, where Apple VoiceProcessingIO
// can provide full-duplex acoustic echo cancellation.
func (a *AudioIO) StartVPIO() error {
	return errors.New("vpio audio backend requires macOS with cgo")
}

func (a *AudioIO) clearVPIOBuffers() {}

type vpioDebugStats struct {
	InputCallbacks  uint64
	OutputCallbacks uint64
	InputFrames     uint64
	OutputFrames    uint64
	PlayUnderruns   uint64
	PlayOverwrites  uint64
	PlayBuffered    int
	PlayCapacity    int
	ForwardedFrames uint64
	GateDropped     uint64
	BargePassed     uint64
	MaxInputLevel   float64
	MaxOutputLevel  float64
}

func (a *AudioIO) vpioDebugStats() vpioDebugStats { return vpioDebugStats{} }
