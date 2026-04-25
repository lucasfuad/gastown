package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestInstallForRole_ConcurrentSpawnsProduceValidJSON covers gh#3500: when
// multiple polecats spawn at the same time they all call InstallForRole on
// the same shared settings file. The previous implementation used
// os.WriteFile (open with O_TRUNC then write); on the affected platforms an
// observer between truncate and write saw a partial JSON file that Claude
// rejected at startup.
//
// With atomic writes (temp + rename), the final settings.json is always a
// well-formed copy of one writer's full output.
//
// Note: the exact corruption reported in the issue is timing-sensitive and
// may not reproduce on every filesystem (single-syscall writes ≤ a few KB
// often serialize at the OS layer on Linux tmpfs). This test asserts the
// post-condition contract — N concurrent writers leave a valid JSON file
// matching the template — which is what atomic-via-rename guarantees.
func TestInstallForRole_ConcurrentSpawnsProduceValidJSON(t *testing.T) {
	dir := t.TempDir()
	const concurrency = 64

	// Pre-create the target file with content that differs from the template,
	// so every writer takes the write path (not the "content equal, skip"
	// early-return). This forces the truncate+write race that gh#3500
	// describes when N polecats race to install settings.json simultaneously.
	dotClaude := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(dotClaude, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(dotClaude, "settings.json")
	// Seed with the legacy `export PATH=` marker so InstallForRole's
	// needsUpgrade() check fires for every writer and they all proceed past
	// the early-return into the racy write path (gh#3500).
	if err := os.WriteFile(target, []byte(`{"stale":true,"hint":"export PATH=/foo"}`), 0600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	// Release all goroutines simultaneously to maximize overlap on the
	// truncate+write window.
	start := make(chan struct{})
	var ready, wg sync.WaitGroup
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		ready.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			if err := InstallForRole("claude", dir, dir, "polecat", ".claude", "settings.json", true); err != nil {
				errs <- err
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("InstallForRole: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}

	// The file must be parseable JSON — corruption from interleaved truncates
	// would produce a syntax error here.
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("settings.json is not valid JSON after concurrent writes: %v\n--- file contents (%d bytes) ---\n%s", err, len(data), string(data))
	}

	// And it must match the resolved template byte-for-byte.
	want, err := resolveAndSubstitute("claude", "settings-autonomous.json", "polecat")
	if err != nil {
		t.Fatalf("resolveAndSubstitute: %v", err)
	}
	if string(data) != string(want) {
		t.Errorf("settings.json content mismatch after concurrent writes: got %d bytes, want %d bytes", len(data), len(want))
	}
}
