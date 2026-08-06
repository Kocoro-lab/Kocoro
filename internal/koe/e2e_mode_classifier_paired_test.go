//go:build darwin && cgo

package koe

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const modeClassifierOutputDirEnv = "KOE_MODE_CLASSIFIER_OUTPUT_DIR"
const modeClassifierCandidateEnv = "KOE_MODE_CLASSIFIER_CANDIDATE"

// TestKoeModeClassifierPairedTextE2E executes control and candidate adjacent
// to one another for every case/repeat pair. Variant order is balanced within
// each repeat and the case order is deterministically shuffled by seed.
func TestKoeModeClassifierPairedTextE2E(t *testing.T) {
	if os.Getenv(modeClassifierGate) != "1" {
		t.Skip("paid Realtime paired mode matrix: set KOE_MODE_CLASSIFIER_E2E=1")
	}
	repeats, err := modeClassifierEnvInt(modeClassifierRepeatsEnv, modeClassifierDefaultRepeat)
	if err != nil {
		t.Fatal(err)
	}
	if repeats < 3 {
		t.Fatalf("%s=%d; paired qualification requires at least 3 repeats", modeClassifierRepeatsEnv, repeats)
	}
	caseTimeout, err := modeClassifierEnvDuration(modeClassifierTimeoutEnv, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := modeClassifierEnvInt64(modeClassifierSeedEnv, modeClassifierDefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	candidate := strings.TrimSpace(os.Getenv(modeClassifierCandidateEnv))
	if candidate == "" {
		candidate = modeClassifierVariantInstructionsOnly
	}
	candidate, err = modeClassifierVariant(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if candidate == modeClassifierVariantBaseline {
		t.Fatalf("%s must name a non-baseline variant", modeClassifierCandidateEnv)
	}
	outputDir := strings.TrimSpace(os.Getenv(modeClassifierOutputDirEnv))
	if outputDir == "" {
		outputDir = filepath.Join(os.TempDir(), "koe-mode-classifier-paired")
	}

	startedAt := time.Now()
	variantTrials := map[string][]modeClassifierTrial{
		modeClassifierVariantBaseline: nil,
		candidate:                     nil,
	}
	totalTrials := 2 * repeats * len(modeClassifierCases)
	suiteTimeout := time.Duration(totalTrials)*caseTimeout + time.Duration(repeats)*time.Minute
	suiteCtx, suiteCancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer suiteCancel()

	executionIndex := 0
	for repeat := 1; repeat <= repeats; repeat++ {
		caseOrder := rand.New(rand.NewSource(seed + int64(repeat))).Perm(len(modeClassifierCases))
		for pairOrder, caseIndex := range caseOrder {
			tc := modeClassifierCases[caseIndex]
			variants := modeClassifierPairedVariantOrder(seed, repeat, pairOrder, candidate)
			for variantOrder, variant := range variants {
				executionIndex++
				trialStarted := time.Now().UTC()
				caseCtx, cancel := context.WithTimeout(suiteCtx, caseTimeout)
				session, connectErr := newModeClassifierSessionForVariant(caseCtx, variant)
				var trial modeClassifierTrial
				if connectErr != nil {
					trial = unknownModeClassifierTrial(
						tc,
						repeat,
						pairOrder+1,
						fmt.Sprintf("connect Realtime: %v", connectErr),
					)
					trial.Variant = variant
					trial.StartedAt = trialStarted
					trial.FinishedAt = time.Now().UTC()
				} else {
					trial = session.classify(caseCtx, tc, repeat, pairOrder+1)
					session.Close()
				}
				cancel()
				trial.PairOrder = pairOrder + 1
				trial.VariantOrder = variantOrder + 1
				trial.ExecutionIndex = executionIndex
				variantTrials[variant] = append(variantTrials[variant], trial)
				t.Logf(
					"repeat=%d pair=%d variant_order=%d execution=%d case=%s variant=%s expected=%s/%s selector=%s/%s observed=%s/%s correct=%v decision=%dms done=%dms error=%q",
					repeat,
					pairOrder+1,
					variantOrder+1,
					executionIndex,
					tc.ID,
					variant,
					tc.Expected,
					tc.ExpectedReason,
					trial.SelectorMode,
					trial.SelectorFullReason,
					trial.Observed,
					trial.ObservedFullReason,
					trial.Correct,
					trial.DecisionLatencyMS,
					trial.ResponseDoneLatency,
					trial.Error,
				)
			}
		}
	}

	unknownTotal := 0
	for _, variant := range []string{modeClassifierVariantBaseline, candidate} {
		report := buildModeClassifierReportForVariant(
			startedAt,
			repeats,
			seed,
			caseTimeout,
			variant,
			variantTrials[variant],
		)
		path := filepath.Join(outputDir, variant+".json")
		if err := writeModeClassifierReport(path, report); err != nil {
			t.Fatalf("write %s mode classifier report: %v", variant, err)
		}
		unknownTotal += report.UnknownTrials
		t.Logf(
			"paired report=%s variant=%s attempts=%d correct=%d unknown=%d false_fast=%d false_full=%d aggregate=%.3f tokens=%d behavior_passed=%v",
			path,
			report.Variant,
			report.TrialCount,
			report.CorrectTrials,
			report.UnknownTrials,
			report.FalseFastTrials,
			report.FalseFullTrials,
			report.AggregateAccuracy,
			report.TotalTokens,
			report.Passed,
		)
	}
	if unknownTotal != 0 {
		t.Fatalf("paired qualification produced %d unknown trials", unknownTotal)
	}
}

func TestModeClassifierPairedOrderIsBalancedAndDeterministic(t *testing.T) {
	for repeat := 1; repeat <= 3; repeat++ {
		baselineFirst := 0
		candidateFirst := 0
		for pairOrder := range modeClassifierCases {
			first := modeClassifierPairedVariantOrder(
				modeClassifierDefaultSeed,
				repeat,
				pairOrder,
				modeClassifierVariantInstructionsOnly,
			)
			second := modeClassifierPairedVariantOrder(
				modeClassifierDefaultSeed,
				repeat,
				pairOrder,
				modeClassifierVariantInstructionsOnly,
			)
			if first[0] != second[0] || first[1] != second[1] {
				t.Fatalf("repeat=%d pair=%d order is not deterministic", repeat, pairOrder+1)
			}
			switch first[0] {
			case modeClassifierVariantBaseline:
				baselineFirst++
			case modeClassifierVariantInstructionsOnly:
				candidateFirst++
			default:
				t.Fatalf("unexpected first variant %q", first[0])
			}
		}
		if baselineFirst != len(modeClassifierCases)/2 || candidateFirst != len(modeClassifierCases)/2 {
			t.Fatalf(
				"repeat=%d first-order balance baseline/candidate=%d/%d, want %d/%d",
				repeat,
				baselineFirst,
				candidateFirst,
				len(modeClassifierCases)/2,
				len(modeClassifierCases)/2,
			)
		}
	}
}

func modeClassifierPairedVariantOrder(
	seed int64,
	repeat int,
	pairOrder int,
	candidate string,
) []string {
	variants := []string{modeClassifierVariantBaseline, candidate}
	if (pairOrder+repeat+int(seed&1))%2 == 0 {
		variants[0], variants[1] = variants[1], variants[0]
	}
	return variants
}
