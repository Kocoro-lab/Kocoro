package claudecode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManifest_IntentReplacedByApplied(t *testing.T) {
	target := t.TempDir()
	p := &Plan{ID: "mig-2026-05-14-test", CreatedAt: time.Now()}

	staging := filepath.Join(target, "skills.staging-a")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := WriteIntentManifest(target, p, []string{staging}); err != nil {
		t.Fatalf("WriteIntentManifest: %v", err)
	}
	intentPath := filepath.Join(target, ".migrate-manifests", p.ID+".intent.json")
	if _, err := os.Stat(intentPath); err != nil {
		t.Fatalf("intent manifest missing after write: %v", err)
	}

	items := []AppliedItem{{Category: "skills", Name: "x", OutputPath: filepath.Join(target, "skills", "x")}}
	if err := WriteAppliedManifest(target, p, items); err != nil {
		t.Fatalf("WriteAppliedManifest: %v", err)
	}
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Error("intent manifest should be removed after applied write")
	}
	appliedPath := filepath.Join(target, ".migrate-manifests", p.ID+".applied.json")
	if _, err := os.Stat(appliedPath); err != nil {
		t.Errorf("applied manifest missing: %v", err)
	}
}

func TestRecoverOrphans_CleansStagingAndRenames(t *testing.T) {
	target := t.TempDir()
	staging := filepath.Join(target, "skills.staging-orphan")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	// File inside the staging dir to prove the whole tree is removed.
	if err := os.WriteFile(filepath.Join(staging, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Plan{ID: "mig-2026-05-14-orphan", CreatedAt: time.Now()}
	if err := WriteIntentManifest(target, p, []string{staging}); err != nil {
		t.Fatal(err)
	}
	// Simulate crash: applied manifest never written. Recovery scan must
	// clean the staging tree and rename .intent → .orphan for audit.
	if err := RecoverOrphans(target); err != nil {
		t.Fatalf("RecoverOrphans: %v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Error("staging dir should be removed by recovery")
	}
	intentPath := filepath.Join(target, ".migrate-manifests", p.ID+".intent.json")
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Error("intent manifest should be renamed to orphan")
	}
	orphanPath := filepath.Join(target, ".migrate-manifests", p.ID+".orphan.json")
	if _, err := os.Stat(orphanPath); err != nil {
		t.Errorf("orphan manifest missing: %v", err)
	}
}

func TestRecoverOrphans_LeavesAppliedAlone(t *testing.T) {
	target := t.TempDir()
	p := &Plan{ID: "mig-2026-05-14-clean", CreatedAt: time.Now()}

	// Write intent then applied — the normal happy-path sequence.
	if err := WriteIntentManifest(target, p, nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteAppliedManifest(target, p, []AppliedItem{}); err != nil {
		t.Fatal(err)
	}

	// Recovery should not touch the applied manifest.
	if err := RecoverOrphans(target); err != nil {
		t.Fatalf("RecoverOrphans: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".migrate-manifests", p.ID+".applied.json")); err != nil {
		t.Error("applied manifest should still exist after recovery")
	}
	if _, err := os.Stat(filepath.Join(target, ".migrate-manifests", p.ID+".orphan.json")); !os.IsNotExist(err) {
		t.Error("recovery should not create orphan when applied exists")
	}
}

func TestRecoverOrphans_NoManifestDir_NoOp(t *testing.T) {
	target := t.TempDir() // no .migrate-manifests subdir
	if err := RecoverOrphans(target); err != nil {
		t.Errorf("recovery on empty target should not error: %v", err)
	}
}
