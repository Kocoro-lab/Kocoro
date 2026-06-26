package koe

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AudioToolbox -framework AudioUnit -framework CoreAudio -framework Foundation
#include <AudioToolbox/AudioToolbox.h>
#include <AudioUnit/AudioUnit.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>

// vpioRing is a mutex-guarded SInt16 ring bridging the realtime VPIO render
// callback (which must never call Go) and the Go capture/playback goroutines.
typedef struct {
    SInt16 *buf;
    int cap;
    int r;
    int w;
    int count;
    pthread_mutex_t mu;
} vpioRing;

static void ringInit(vpioRing *ring, int cap) {
    ring->buf = (SInt16 *)calloc(cap, sizeof(SInt16));
    ring->cap = cap;
    ring->r = 0;
    ring->w = 0;
    ring->count = 0;
    pthread_mutex_init(&ring->mu, NULL);
}

static void ringFree(vpioRing *ring) {
    if (ring->buf) { free(ring->buf); ring->buf = NULL; }
    pthread_mutex_destroy(&ring->mu);
}

// ringWrite appends n samples, overwriting the oldest on overflow.
static void ringWrite(vpioRing *ring, const SInt16 *src, int n) {
    pthread_mutex_lock(&ring->mu);
    for (int i = 0; i < n; i++) {
        ring->buf[ring->w] = src[i];
        ring->w = (ring->w + 1) % ring->cap;
        if (ring->count < ring->cap) {
            ring->count++;
        } else {
            ring->r = (ring->r + 1) % ring->cap;
        }
    }
    pthread_mutex_unlock(&ring->mu);
}

// ringRead reads up to n samples, zero-filling the remainder. Returns samples read.
static int ringRead(vpioRing *ring, SInt16 *dst, int n) {
    pthread_mutex_lock(&ring->mu);
    int got = 0;
    while (got < n && ring->count > 0) {
        dst[got++] = ring->buf[ring->r];
        ring->r = (ring->r + 1) % ring->cap;
        ring->count--;
    }
    pthread_mutex_unlock(&ring->mu);
    for (int i = got; i < n; i++) {
        dst[i] = 0;
    }
    return got;
}

static int ringCount(vpioRing *ring) {
    pthread_mutex_lock(&ring->mu);
    int c = ring->count;
    pthread_mutex_unlock(&ring->mu);
    return c;
}

static AudioUnit gVAU = 0;
static vpioRing gMicRing;
static vpioRing gPlayRing;

// vpioRenderCB is VPIO's output callback: it pulls the AEC'd mic (input bus 1)
// into ioData, publishes it to the mic ring for Go, then replaces the buffer with
// queued playback from the play ring (or silence) for the speaker.
static OSStatus vpioRenderCB(void *inRefCon, AudioUnitRenderActionFlags *flags,
                             const AudioTimeStamp *ts, UInt32 bus, UInt32 nFrames,
                             AudioBufferList *ioData) {
    OSStatus st = AudioUnitRender(gVAU, flags, ts, 1, nFrames, ioData);
    if (st != noErr) return st;
    if (ioData->mNumberBuffers > 0) {
        SInt16 *s = (SInt16 *)ioData->mBuffers[0].mData;
        int n = ioData->mBuffers[0].mDataByteSize / sizeof(SInt16);
        ringWrite(&gMicRing, s, n);
        ringRead(&gPlayRing, s, n);
    }
    return noErr;
}

static OSStatus vpioStartC(double sampleRate, int ringCap) {
    ringInit(&gMicRing, ringCap);
    ringInit(&gPlayRing, ringCap);
    AudioComponentDescription desc = {0};
    desc.componentType = kAudioUnitType_Output;
    desc.componentSubType = kAudioUnitSubType_VoiceProcessingIO;
    desc.componentManufacturer = kAudioUnitManufacturer_Apple;
    AudioComponent comp = AudioComponentFindNext(NULL, &desc);
    if (!comp) return -1;
    OSStatus st = AudioComponentInstanceNew(comp, &gVAU);
    if (st != noErr) return st;

    UInt32 one = 1;
    st = AudioUnitSetProperty(gVAU, kAudioOutputUnitProperty_EnableIO, kAudioUnitScope_Input, 1, &one, sizeof(one));
    if (st != noErr) return st;
    st = AudioUnitSetProperty(gVAU, kAudioOutputUnitProperty_EnableIO, kAudioUnitScope_Output, 0, &one, sizeof(one));
    if (st != noErr) return st;

    AudioStreamBasicDescription fmt = {0};
    fmt.mSampleRate = sampleRate;
    fmt.mFormatID = kAudioFormatLinearPCM;
    fmt.mFormatFlags = kAudioFormatFlagIsSignedInteger | kAudioFormatFlagIsPacked;
    fmt.mFramesPerPacket = 1;
    fmt.mChannelsPerFrame = 1;
    fmt.mBitsPerChannel = 16;
    fmt.mBytesPerFrame = 2;
    fmt.mBytesPerPacket = 2;
    st = AudioUnitSetProperty(gVAU, kAudioUnitProperty_StreamFormat, kAudioUnitScope_Output, 1, &fmt, sizeof(fmt));
    if (st != noErr) return st;
    st = AudioUnitSetProperty(gVAU, kAudioUnitProperty_StreamFormat, kAudioUnitScope_Input, 0, &fmt, sizeof(fmt));
    if (st != noErr) return st;

    AURenderCallbackStruct cb = {0};
    cb.inputProc = vpioRenderCB;
    st = AudioUnitSetProperty(gVAU, kAudioUnitProperty_SetRenderCallback, kAudioUnitScope_Input, 0, &cb, sizeof(cb));
    if (st != noErr) return st;

    st = AudioUnitInitialize(gVAU);
    if (st != noErr) return st;
    return AudioOutputUnitStart(gVAU);
}

static void vpioStopC(void) {
    if (gVAU) {
        AudioOutputUnitStop(gVAU);
        AudioUnitUninitialize(gVAU);
        AudioComponentInstanceDispose(gVAU);
        gVAU = 0;
    }
    ringFree(&gMicRing);
    ringFree(&gPlayRing);
}

static int vpioReadMic(SInt16 *dst, int n) { return ringRead(&gMicRing, dst, n); }
static void vpioWritePlay(SInt16 *src, int n) { ringWrite(&gPlayRing, src, n); }
static int vpioMicCount(void) { return ringCount(&gMicRing); }
*/
import "C"

import (
	"fmt"
	"time"
	"unsafe"
)

// StartVPIO opens the VoiceProcessingIO device (Apple's terminal AEC, full-duplex
// barge-in) as an alternative to malgo's Start(). The render callback (C, realtime
// thread) cannot call Go, so it bridges through two C ring buffers: it publishes
// AEC'd mic to gMicRing and reads gPlayRing for the speaker; two Go goroutines
// drain gMicRing → a.frames (960-sample frames) and a.playBuf → gPlayRing. With
// VPIO the half-duplex gate is moot — VPIO cancels the echo natively, enabling
// barge-in. De-risked in koe-spike/stage4-vpio-aec; runtime AEC quality is a
// live/by-ear check (Connect chooses Start vs StartVPIO).
func (a *AudioIO) StartVPIO() error {
	const ringCap = audioSampleRate / 2 // ~500ms of mono S16 per direction
	if st := C.vpioStartC(C.double(audioSampleRate), C.int(ringCap)); st != 0 {
		return fmt.Errorf("vpio start: OSStatus %d", int(st))
	}
	a.vpioActive = true
	a.vpioDone = make(chan struct{})
	go a.vpioCaptureLoop()
	go a.vpioPlaybackLoop()
	return nil
}

func (a *AudioIO) stopVPIO() {
	if a.vpioDone != nil {
		close(a.vpioDone)
	}
	C.vpioStopC()
}

// vpioCaptureLoop drains the C mic ring into a.frames in 960-sample (20 ms) frames.
func (a *AudioIO) vpioCaptureLoop() {
	ticker := time.NewTicker(audioFrameMs * time.Millisecond / 2) // poll at 2x the frame rate
	defer ticker.Stop()
	for {
		select {
		case <-a.vpioDone:
			return
		case <-ticker.C:
		}
		for int(C.vpioMicCount()) >= audioFrameSize {
			frame := make([]int16, audioFrameSize)
			n := int(C.vpioReadMic((*C.SInt16)(unsafe.Pointer(&frame[0])), C.int(audioFrameSize)))
			if n == 0 {
				break
			}
			select {
			case a.frames <- frame:
			default: // drop if the send path is behind
			}
		}
	}
}

// vpioPlaybackLoop drains a.playBuf into the C play ring for the render callback.
func (a *AudioIO) vpioPlaybackLoop() {
	for {
		select {
		case <-a.vpioDone:
			return
		case pcm := <-a.playBuf:
			if len(pcm) > 0 {
				C.vpioWritePlay((*C.SInt16)(unsafe.Pointer(&pcm[0])), C.int(len(pcm)))
			}
		}
	}
}
