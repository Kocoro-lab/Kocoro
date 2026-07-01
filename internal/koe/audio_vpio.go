//go:build darwin && cgo

package koe

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AudioToolbox -framework AudioUnit -framework CoreAudio -framework Foundation
#include <AudioToolbox/AudioToolbox.h>
#include <AudioUnit/AudioUnit.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
    SInt16 *buf;
    int cap;
    int r;
    int w;
    int count;
    int initialized;
    unsigned long long *overwrites;
    pthread_mutex_t mu;
} vpioRing;

static void ringInit(vpioRing *ring, int cap, unsigned long long *overwrites) {
    ring->buf = (SInt16 *)calloc(cap, sizeof(SInt16));
    ring->cap = cap;
    ring->r = 0;
    ring->w = 0;
    ring->count = 0;
    ring->initialized = 1;
    ring->overwrites = overwrites;
    pthread_mutex_init(&ring->mu, NULL);
}

static void ringFree(vpioRing *ring) {
    if (!ring->initialized) return;
    if (ring->buf) {
        free(ring->buf);
        ring->buf = NULL;
    }
    ring->cap = 0;
    ring->r = 0;
    ring->w = 0;
    ring->count = 0;
    ring->initialized = 0;
    ring->overwrites = NULL;
    pthread_mutex_destroy(&ring->mu);
}

static void ringWrite(vpioRing *ring, const SInt16 *src, int n) {
    if (!ring->initialized || !ring->buf || ring->cap <= 0) return;
    pthread_mutex_lock(&ring->mu);
    for (int i = 0; i < n; i++) {
        ring->buf[ring->w] = src[i];
        ring->w = (ring->w + 1) % ring->cap;
        if (ring->count < ring->cap) {
            ring->count++;
        } else {
            ring->r = (ring->r + 1) % ring->cap;
            if (ring->overwrites) (*ring->overwrites)++;
        }
    }
    pthread_mutex_unlock(&ring->mu);
}

static int ringRead(vpioRing *ring, SInt16 *dst, int n) {
    if (!ring->initialized || !ring->buf || ring->cap <= 0) {
        for (int i = 0; i < n; i++) dst[i] = 0;
        return 0;
    }
    pthread_mutex_lock(&ring->mu);
    int got = 0;
    while (got < n && ring->count > 0) {
        dst[got++] = ring->buf[ring->r];
        ring->r = (ring->r + 1) % ring->cap;
        ring->count--;
    }
    pthread_mutex_unlock(&ring->mu);
    for (int i = got; i < n; i++) dst[i] = 0;
    return got;
}

static int ringCount(vpioRing *ring) {
    if (!ring->initialized) return 0;
    pthread_mutex_lock(&ring->mu);
    int c = ring->count;
    pthread_mutex_unlock(&ring->mu);
    return c;
}

static void ringClear(vpioRing *ring) {
    if (!ring->initialized) return;
    pthread_mutex_lock(&ring->mu);
    ring->r = 0;
    ring->w = 0;
    ring->count = 0;
    pthread_mutex_unlock(&ring->mu);
}

static AudioUnit gVAU = 0;
static vpioRing gMicRing;
static vpioRing gPlayRing;
static Float32 *gInputFloatScratch = NULL;
static SInt16 *gInputIntScratch = NULL;
static SInt16 *gOutputScratch = NULL;
static int gInputScratchCap = 0;
static int gPlayPrimed = 0;
static int gPlayPrerollSamples = 0;
static unsigned long long gInputCallbacks = 0;
static unsigned long long gOutputCallbacks = 0;
static unsigned long long gInputFrames = 0;
static unsigned long long gOutputFrames = 0;
static unsigned long long gPlayUnderruns = 0;
static unsigned long long gPlayOverwrites = 0;

static int vpioProbeEnabled(void) {
    const char *v = getenv("KOE_VPIO_PROBE");
    return v && v[0] && strcmp(v, "0") != 0;
}

static void vpioProbe(const char *step) {
    if (!vpioProbeEnabled()) return;
    fprintf(stderr, "koe[vpio]: %s\n", step);
    fflush(stderr);
}

static void zeroABL(AudioBufferList *ioData) {
    if (!ioData) return;
    for (UInt32 i = 0; i < ioData->mNumberBuffers; i++) {
        if (ioData->mBuffers[i].mData && ioData->mBuffers[i].mDataByteSize > 0) {
            memset(ioData->mBuffers[i].mData, 0, ioData->mBuffers[i].mDataByteSize);
        }
    }
}

static void vpioFreeScratchC(void) {
    if (gInputFloatScratch) {
        free(gInputFloatScratch);
        gInputFloatScratch = NULL;
    }
    if (gInputIntScratch) {
        free(gInputIntScratch);
        gInputIntScratch = NULL;
    }
    if (gOutputScratch) {
        free(gOutputScratch);
        gOutputScratch = NULL;
    }
    gInputScratchCap = 0;
}

static OSStatus vpioInputCB(void *inRefCon, AudioUnitRenderActionFlags *flags,
                            const AudioTimeStamp *ts, UInt32 bus, UInt32 nFrames,
                            AudioBufferList *ioData) {
    if (!gVAU || !gInputFloatScratch || !gInputIntScratch || (int)nFrames > gInputScratchCap) return noErr;
    AudioBufferList abl;
    abl.mNumberBuffers = 1;
    abl.mBuffers[0].mNumberChannels = 1;
    abl.mBuffers[0].mDataByteSize = nFrames * sizeof(Float32);
    abl.mBuffers[0].mData = gInputFloatScratch;
    OSStatus st = AudioUnitRender(gVAU, flags, ts, 1, nFrames, &abl);
    if (st != noErr) return st;
    for (UInt32 i = 0; i < nFrames; i++) {
        Float32 x = gInputFloatScratch[i];
        if (x > 1.0f) x = 1.0f;
        if (x < -1.0f) x = -1.0f;
        gInputIntScratch[i] = (SInt16)(x * 32767.0f);
    }
    gInputCallbacks++;
    gInputFrames += nFrames;
    ringWrite(&gMicRing, gInputIntScratch, (int)nFrames);
    return noErr;
}

static OSStatus vpioOutputCB(void *inRefCon, AudioUnitRenderActionFlags *flags,
                             const AudioTimeStamp *ts, UInt32 bus, UInt32 nFrames,
                             AudioBufferList *ioData) {
    gOutputCallbacks++;
    gOutputFrames += nFrames;
    if (!ioData) return noErr;
    if (!gPlayPrimed) {
        if (ringCount(&gPlayRing) < gPlayPrerollSamples) {
            zeroABL(ioData);
            if (flags) *flags |= kAudioUnitRenderAction_OutputIsSilence;
            return noErr;
        }
        gPlayPrimed = 1;
    }
    if (!gOutputScratch || (int)nFrames > gInputScratchCap) {
        zeroABL(ioData);
        if (flags) *flags |= kAudioUnitRenderAction_OutputIsSilence;
        return noErr;
    }
    int got = ringRead(&gPlayRing, gOutputScratch, (int)nFrames);
    if (got < (int)nFrames) {
        gPlayPrimed = 0;
        gPlayUnderruns++;
        if (got == 0 && flags) *flags |= kAudioUnitRenderAction_OutputIsSilence;
    } else if (flags) {
        *flags &= ~kAudioUnitRenderAction_OutputIsSilence;
    }
    for (UInt32 i = 0; i < ioData->mNumberBuffers; i++) {
        Float32 *f = (Float32 *)ioData->mBuffers[i].mData;
        int n = ioData->mBuffers[i].mDataByteSize / sizeof(Float32);
        if (!f) continue;
        for (int j = 0; j < n; j++) {
            SInt16 sample = 0;
            if (j < (int)nFrames) {
                sample = gOutputScratch[j];
            }
            f[j] = ((Float32)sample) / 32768.0f;
        }
    }
    return noErr;
}

static void vpioStopUnitC(void) {
    if (gVAU) {
        AudioOutputUnitStop(gVAU);
        AudioUnitUninitialize(gVAU);
        AudioComponentInstanceDispose(gVAU);
        gVAU = 0;
    }
}

static void vpioFreeRingsC(void) {
    ringFree(&gMicRing);
    ringFree(&gPlayRing);
    vpioFreeScratchC();
}

static void vpioCleanupC(void) {
    vpioStopUnitC();
    vpioFreeRingsC();
}

static OSStatus vpioStartC(double sampleRate, int ringCap, int prerollSamples) {
    vpioProbe("start enter");
    ringInit(&gMicRing, ringCap, NULL);
    ringInit(&gPlayRing, ringCap, &gPlayOverwrites);
    gInputFloatScratch = (Float32 *)calloc(ringCap, sizeof(Float32));
    gInputIntScratch = (SInt16 *)calloc(ringCap, sizeof(SInt16));
    gOutputScratch = (SInt16 *)calloc(ringCap, sizeof(SInt16));
    gInputScratchCap = ringCap;
    gPlayPrimed = 0;
    gPlayPrerollSamples = prerollSamples;
    gInputCallbacks = 0;
    gOutputCallbacks = 0;
    gInputFrames = 0;
    gOutputFrames = 0;
    gPlayUnderruns = 0;
    gPlayOverwrites = 0;
    if (!gInputFloatScratch || !gInputIntScratch || !gOutputScratch) {
        vpioCleanupC();
        return -2;
    }

    AudioComponentDescription desc = {0};
    desc.componentType = kAudioUnitType_Output;
    desc.componentSubType = kAudioUnitSubType_VoiceProcessingIO;
    desc.componentManufacturer = kAudioUnitManufacturer_Apple;
    AudioComponent comp = AudioComponentFindNext(NULL, &desc);
    if (!comp) {
        vpioCleanupC();
        return -1;
    }
    vpioProbe("component found");
    vpioProbe("AudioComponentInstanceNew begin");
    OSStatus st = AudioComponentInstanceNew(comp, &gVAU);
    if (st != noErr) {
        vpioCleanupC();
        return st;
    }
    vpioProbe("AudioComponentInstanceNew done");

    UInt32 one = 1;
    UInt32 zero = 0;
    vpioProbe("EnableIO input begin");
    st = AudioUnitSetProperty(gVAU, kAudioOutputUnitProperty_EnableIO, kAudioUnitScope_Input, 1, &one, sizeof(one));
    if (st != noErr) { vpioCleanupC(); return st; }
    vpioProbe("EnableIO input done");
    vpioProbe("EnableIO output begin");
    st = AudioUnitSetProperty(gVAU, kAudioOutputUnitProperty_EnableIO, kAudioUnitScope_Output, 0, &one, sizeof(one));
    if (st != noErr) { vpioCleanupC(); return st; }
    vpioProbe("EnableIO output done");
    vpioProbe("BypassVoiceProcessing begin");
    st = AudioUnitSetProperty(gVAU, kAUVoiceIOProperty_BypassVoiceProcessing, kAudioUnitScope_Global, 0, &zero, sizeof(zero));
    if (st != noErr) { vpioCleanupC(); return st; }
    vpioProbe("BypassVoiceProcessing done");
    vpioProbe("VoiceProcessingEnableAGC begin");
    st = AudioUnitSetProperty(gVAU, kAUVoiceIOProperty_VoiceProcessingEnableAGC, kAudioUnitScope_Global, 0, &one, sizeof(one));
    if (st != noErr) { vpioCleanupC(); return st; }
    vpioProbe("VoiceProcessingEnableAGC done");

    AudioStreamBasicDescription fmt = {0};
    fmt.mSampleRate = sampleRate;
    fmt.mFormatID = kAudioFormatLinearPCM;
    fmt.mFormatFlags = kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked;
    fmt.mFramesPerPacket = 1;
    fmt.mChannelsPerFrame = 1;
    fmt.mBitsPerChannel = 32;
    fmt.mBytesPerFrame = 4;
    fmt.mBytesPerPacket = 4;
    vpioProbe("StreamFormat input-side begin");
    st = AudioUnitSetProperty(gVAU, kAudioUnitProperty_StreamFormat, kAudioUnitScope_Output, 1, &fmt, sizeof(fmt));
    if (st != noErr) { vpioCleanupC(); return st; }
    vpioProbe("StreamFormat input-side done");
    vpioProbe("StreamFormat output-side begin");
    st = AudioUnitSetProperty(gVAU, kAudioUnitProperty_StreamFormat, kAudioUnitScope_Input, 0, &fmt, sizeof(fmt));
    if (st != noErr) { vpioCleanupC(); return st; }
    vpioProbe("StreamFormat output-side done");

    AURenderCallbackStruct inputCB = {0};
    inputCB.inputProc = vpioInputCB;
    vpioProbe("SetInputCallback begin");
    st = AudioUnitSetProperty(gVAU, kAudioOutputUnitProperty_SetInputCallback, kAudioUnitScope_Global, 1, &inputCB, sizeof(inputCB));
    if (st != noErr) { vpioCleanupC(); return st; }
    vpioProbe("SetInputCallback done");

    AURenderCallbackStruct outputCB = {0};
    outputCB.inputProc = vpioOutputCB;
    vpioProbe("SetRenderCallback begin");
    st = AudioUnitSetProperty(gVAU, kAudioUnitProperty_SetRenderCallback, kAudioUnitScope_Input, 0, &outputCB, sizeof(outputCB));
    if (st != noErr) { vpioCleanupC(); return st; }
    vpioProbe("SetRenderCallback done");

    vpioProbe("AudioUnitInitialize begin");
    st = AudioUnitInitialize(gVAU);
    if (st != noErr) { vpioCleanupC(); return st; }
    vpioProbe("AudioUnitInitialize done");
    vpioProbe("AudioOutputUnitStart begin");
    st = AudioOutputUnitStart(gVAU);
    if (st != noErr) { vpioCleanupC(); return st; }
    vpioProbe("AudioOutputUnitStart done");
    return noErr;
}

static int vpioReadMic(SInt16 *dst, int n) { return ringRead(&gMicRing, dst, n); }
static void vpioWritePlay(SInt16 *src, int n) { ringWrite(&gPlayRing, src, n); }
static void vpioClearBuffers(void) {
    ringClear(&gMicRing);
    ringClear(&gPlayRing);
    gPlayPrimed = 0;
}
static int vpioMicCount(void) { return ringCount(&gMicRing); }
static int vpioPlayCount(void) { return ringCount(&gPlayRing); }
static int vpioPlayCapacity(void) { return gPlayRing.cap; }
static unsigned long long vpioInputCallbackCount(void) { return gInputCallbacks; }
static unsigned long long vpioOutputCallbackCount(void) { return gOutputCallbacks; }
static unsigned long long vpioInputFrameCount(void) { return gInputFrames; }
static unsigned long long vpioOutputFrameCount(void) { return gOutputFrames; }
static unsigned long long vpioPlayUnderrunCount(void) { return gPlayUnderruns; }
static unsigned long long vpioPlayOverwriteCount(void) { return gPlayOverwrites; }
*/
import "C"

import (
	"fmt"
	"math"
	"time"
	"unsafe"
)

const vpioPlaybackHighWaterSamples = audioSampleRate / 2 // keep hardware ring latency around 500 ms

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

// StartVPIO opens Apple's VoiceProcessingIO backend. It is opt-in and macOS-only:
// VPIO provides native echo cancellation, while SetSpeaking still keeps the mic
// quiet during playback by default. Experimental barge-in must opt in.
func (a *AudioIO) StartVPIO() error {
	const ringCap = audioSampleRate * 2 // ~2s of mono S16 per direction
	if st := C.vpioStartC(C.double(audioSampleRate), C.int(ringCap), C.int(prerollFrames*audioFrameSize)); st != 0 {
		return fmt.Errorf("vpio start: OSStatus %d", int(st))
	}
	a.vpioActive = true
	a.vpioDone = make(chan struct{})
	a.vpioWG.Add(2)
	go a.vpioCaptureLoop()
	go a.vpioPlaybackLoop()
	return nil
}

func (a *AudioIO) clearVPIOBuffers() {
	if a.vpioActive {
		C.vpioClearBuffers()
	}
}

func (a *AudioIO) stopVPIO() {
	C.vpioStopUnitC()
	if a.vpioDone != nil {
		close(a.vpioDone)
	}
	a.vpioWG.Wait()
	C.vpioFreeRingsC()
	a.vpioActive = false
	a.vpioDone = nil
}

func (a *AudioIO) vpioCaptureLoop() {
	defer a.vpioWG.Done()
	ticker := time.NewTicker(audioFrameMs * time.Millisecond / 2)
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
			level := rmsLevel(frame)
			a.setInputLevel(level)
			a.trackVPIOMaxInput(level)
			if !a.shouldForwardVPIOCapture(level) {
				continue
			}
			select {
			case a.frames <- frame:
			default:
			}
		}
	}
}

func (a *AudioIO) vpioPlaybackLoop() {
	defer a.vpioWG.Done()
	for {
		select {
		case <-a.vpioDone:
			return
		case pcm := <-a.playBuf:
			if len(pcm) == 0 {
				continue
			}
			if !a.waitForVPIOPlaySpace(len(pcm)) {
				return
			}
			a.setOutputLevel(rmsLevel(pcm))
			C.vpioWritePlay((*C.SInt16)(unsafe.Pointer(&pcm[0])), C.int(len(pcm)))
		}
	}
}

func (a *AudioIO) waitForVPIOPlaySpace(samples int) bool {
	if samples <= 0 {
		return true
	}
	ticker := time.NewTicker(audioFrameMs * time.Millisecond / 2)
	defer ticker.Stop()
	for {
		capacity := int(C.vpioPlayCapacity())
		if capacity <= 0 || samples > capacity {
			return false
		}
		limit := capacity
		if limit > vpioPlaybackHighWaterSamples {
			limit = vpioPlaybackHighWaterSamples
		}
		if samples > limit {
			limit = samples
		}
		if int(C.vpioPlayCount()) <= limit-samples {
			return true
		}
		select {
		case <-a.vpioDone:
			return false
		case <-ticker.C:
		}
	}
}

func (a *AudioIO) vpioDebugStats() vpioDebugStats {
	return vpioDebugStats{
		InputCallbacks:  uint64(C.vpioInputCallbackCount()),
		OutputCallbacks: uint64(C.vpioOutputCallbackCount()),
		InputFrames:     uint64(C.vpioInputFrameCount()),
		OutputFrames:    uint64(C.vpioOutputFrameCount()),
		PlayUnderruns:   uint64(C.vpioPlayUnderrunCount()),
		PlayOverwrites:  uint64(C.vpioPlayOverwriteCount()),
		PlayBuffered:    int(C.vpioPlayCount()),
		PlayCapacity:    int(C.vpioPlayCapacity()),
		ForwardedFrames: a.vpioForwarded.Load(),
		GateDropped:     a.vpioGateDropped.Load(),
		BargePassed:     a.vpioBargePassed.Load(),
		MaxInputLevel:   math.Float64frombits(a.vpioMaxInput.Load()),
		MaxOutputLevel:  math.Float64frombits(a.vpioMaxOutput.Load()),
	}
}
