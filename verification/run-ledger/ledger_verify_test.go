//go:build ledgerverify

// Property checks for Run Ledger event identity across Windows Host Agent
// checkpoints. check.sh copies this file into internal/cli of a scratch copy
// of the repo and runs it with -tags ledgerverify; it is never built in place.
//
// The streams are produced by the real copyBoundedLedgerEvents and merged by
// the real mergeHostAgentProgressEvents / mergeHostAgentTerminalEvents. The
// well-formedness and identity checks below are written from ADR 0048, not
// from the merge code, so they can disagree with it.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const lvGapType = "run-ledger.events.truncated"

// lvLedgerView is what a client reading the stream from sequence 1 learns:
// event identity -> payload, plus the one retention gap.
type lvLedgerView struct {
	events   map[int]string
	gapFirst int
	gapLast  int
	last     int
}

func lvCanonicalJSON(line []byte) (string, map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var event map[string]any
	if err := decoder.Decode(&event); err != nil {
		return "", nil, err
	}
	encoded, err := json.Marshal(event)
	return string(encoded), event, err
}

func lvIntField(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	return int(parsed), err == nil
}

// lvViewOf checks the ADR 0048 stream shape: sequences start at 1 and rise by
// one, except across at most one retention gap marker whose details name
// exactly the sequences it stands for.
func lvViewOf(stream []byte) (lvLedgerView, error) {
	view := lvLedgerView{events: map[int]string{}}
	trimmed := bytes.TrimSpace(stream)
	if len(trimmed) == 0 {
		return view, fmt.Errorf("empty stream")
	}
	expected := 1
	for index, line := range bytes.Split(trimmed, []byte{'\n'}) {
		canonical, event, err := lvCanonicalJSON(line)
		if err != nil {
			return view, fmt.Errorf("line %d: %v", index+1, err)
		}
		sequence, ok := lvIntField(event["sequence"])
		eventType, _ := event["type"].(string)
		if !ok || eventType == "" {
			return view, fmt.Errorf("line %d: missing sequence or type", index+1)
		}
		if sequence != expected {
			return view, fmt.Errorf("line %d: sequence %d, want %d", index+1, sequence, expected)
		}
		if eventType == lvGapType {
			details, _ := event["details"].(map[string]any)
			omitted, okCount := lvIntField(details["omittedCount"])
			first, okFirst := lvIntField(details["firstOmittedSequence"])
			last, okLast := lvIntField(details["lastOmittedSequence"])
			if view.gapFirst != 0 || !okCount || !okFirst || !okLast || omitted < 1 || first != sequence || last != sequence+omitted-1 {
				return view, fmt.Errorf("line %d: invalid gap marker", index+1)
			}
			view.gapFirst, view.gapLast = first, last
			expected = last + 1
			continue
		}
		view.events[sequence] = canonical
		view.last = sequence
		expected++
	}
	return view, nil
}

// lvCoverage counts situations the honest replay reached; each must be
// non-zero or the passes say little (canaries).
var lvCoverage = map[string]int{}

func lvNote(before []byte, after []byte) {
	old, errOld := lvViewOf(before)
	next, errNext := lvViewOf(after)
	if errOld != nil || errNext != nil {
		return
	}
	if next.gapFirst > 0 {
		lvCoverage["stored stream has a retention gap"]++
	}
	if old.gapFirst > 0 && next.gapLast > old.gapLast {
		lvCoverage["gap widened by a later snapshot"]++
	}
	for sequence := range old.events {
		if _, ok := next.events[sequence]; !ok {
			lvCoverage["acknowledged event absorbed into a gap"]++
			break
		}
	}
	if bytes.Contains(after, []byte(`"run-ledger.event.truncated"`)) {
		lvCoverage["oversized event replaced"]++
	}
	if next.last > old.last {
		lvCoverage["new events appended"]++
	}
}

// lvCheckTransition holds for every stored stream `after` that replaced `before`.
func lvCheckTransition(before []byte, after []byte) error {
	old, err := lvViewOf(before)
	if err != nil {
		return fmt.Errorf("before is malformed: %v", err)
	}
	next, err := lvViewOf(after)
	if err != nil {
		return fmt.Errorf("WellFormed: stored stream is malformed: %v", err)
	}
	if next.last < old.last {
		return fmt.Errorf("Monotone: last event sequence went back from %d to %d", old.last, next.last)
	}
	// An acknowledged sequence can only disappear into the new gap: the new
	// stream is contiguous (WellFormed) and reaches at least old.last (Monotone).
	for sequence, payload := range old.events {
		got, present := next.events[sequence]
		if present && got != payload {
			return fmt.Errorf("IdentityStable: sequence %d changed\nbefore: %s\nafter:  %s", sequence, payload, got)
		}
	}
	if old.gapFirst > 0 {
		for sequence := old.gapFirst; sequence <= old.gapLast; sequence++ {
			if _, present := next.events[sequence]; present {
				return fmt.Errorf("NoRestore: sequence %d was inside a published gap [%d,%d] and reappeared", sequence, old.gapFirst, old.gapLast)
			}
		}
	}
	return nil
}

// ---- generators built on the real bounding code ----

type lvLedgerPolicyCase struct {
	progressEvents int
	terminalEvents int
	maxBytes       int
}

func lvRawEvents(n int, big map[int]bool) [][]byte {
	lines := make([][]byte, 0, n)
	for i := 1; i <= n; i++ {
		message := fmt.Sprintf("m%d", i)
		if big[i] {
			message = strings.Repeat("x", 700)
		}
		encoded, _ := json.Marshal(map[string]any{
			"event": fmt.Sprintf("e%d", i), "type": fmt.Sprintf("e%d", i),
			"timestamp": fmt.Sprintf("2026-01-01T00:00:%02dZ", i), "details": map[string]any{"message": message},
		})
		lines = append(lines, encoded)
	}
	return lines
}

// lvBoundCache memoizes copyBoundedLedgerEvents: it is deterministic for a
// given input and policy, and every call pays for fsync.
var lvBoundCache = map[string][]byte{}

func lvBoundEvents(t *testing.T, dir string, content []byte, maxEvents int, maxBytes int) []byte {
	t.Helper()
	key := fmt.Sprintf("%d/%d/%s", maxEvents, maxBytes, content)
	if cached, ok := lvBoundCache[key]; ok {
		return cached
	}
	source := filepath.Join(dir, "source.jsonl")
	destination := filepath.Join(dir, "bounded.jsonl")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := copyBoundedLedgerEvents(source, destination, maxEvents, maxBytes, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("copyBoundedLedgerEvents(max=%d bytes=%d): %v", maxEvents, maxBytes, err)
	}
	bounded, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	lvBoundCache[key] = bounded
	return bounded
}

func lvJoinLines(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	return append(bytes.Join(lines, []byte{'\n'}), '\n')
}

// lvAgentCheckpoint mirrors buildHostAgentProgress: bound the raw events, then
// bound the (sanitized) result again with the same progress policy.
func lvAgentCheckpoint(t *testing.T, dir string, raw [][]byte, policy lvLedgerPolicyCase) []byte {
	first := lvBoundEvents(t, dir, lvJoinLines(raw), policy.progressEvents, policy.maxBytes)
	return lvBoundEvents(t, dir, first, policy.progressEvents, policy.maxBytes)
}

// lvAgentRaw is the Agent's events.jsonl after k events: the assigned prefix
// exactly as installAssignedEventPrefix wrote it, then unsequenced appends.
func lvAgentRaw(prefix []byte, raw [][]byte, prefixLen int, k int) [][]byte {
	lines := bytes.Split(bytes.TrimSpace(prefix), []byte{'\n'})
	return append(append([][]byte{}, lines...), raw[prefixLen:k]...)
}

// lvTerminalStaged mirrors acceptHostAgentCompletion: SyncArtifacts bounds the
// returned terminal events with the Control Plane Run Ledger policy.
func lvTerminalStaged(t *testing.T, dir string, raw [][]byte, policy lvLedgerPolicyCase) []byte {
	return lvBoundEvents(t, dir, lvJoinLines(raw), policy.terminalEvents, policy.maxBytes)
}

func lvNondecreasing(low int, high int, length int) [][]int {
	if length == 0 {
		return [][]int{{}}
	}
	var out [][]int
	for first := low; first <= high; first++ {
		for _, rest := range lvNondecreasing(first, high, length-1) {
			out = append(out, append([]int{first}, rest...))
		}
	}
	return out
}

type lvLedgerScenario struct {
	n           int
	prefix      int
	checkpoints []int
	policy      lvLedgerPolicyCase
	big         map[int]bool
}

func (scenario lvLedgerScenario) String() string {
	var big []int
	for index := range scenario.big {
		big = append(big, index)
	}
	sort.Ints(big)
	return fmt.Sprintf("events=%d prefix=%d checkpoints=%v progressMaxEvents=%d terminalMaxEvents=%d maxEventBytes=%d oversized=%v",
		scenario.n, scenario.prefix, scenario.checkpoints, scenario.policy.progressEvents, scenario.policy.terminalEvents, scenario.policy.maxBytes, big)
}

// runScenario replays one honest Agent run through the real merges. It
// returns the first property violation, or a rejection of an honest
// checkpoint (HonestAccepted), with the streams involved.
func lvReplay(t *testing.T, dir string, scenario lvLedgerScenario) error {
	raw := lvRawEvents(scenario.n, scenario.big)
	// The Control Plane ledger starts from the assigned event prefix.
	stored := lvTerminalStaged(t, dir, raw[:scenario.prefix], scenario.policy)
	view, err := lvViewOf(stored)
	if err != nil {
		return fmt.Errorf("WellFormed: initial ledger malformed: %v\n%s", err, stored)
	}
	if view.gapFirst > 0 {
		// installAssignedEventPrefix refuses a gapped prefix before any
		// checkpoint; the run fails at the Agent instead.
		lvCoverage["gapped prefix refused by Agent (skipped)"]++
		return nil
	}
	prefix := stored
	for index, k := range scenario.checkpoints {
		incoming := lvAgentCheckpoint(t, dir, lvAgentRaw(prefix, raw, scenario.prefix, k), scenario.policy)
		merged, err := mergeHostAgentProgressEvents(stored, incoming)
		if err != nil {
			return fmt.Errorf("HonestAccepted: checkpoint %d (events 1..%d) rejected: %v\nstored:\n%sincoming:\n%s", index+1, k, err, stored, incoming)
		}
		lvNote(stored, merged)
		if err := lvCheckTransition(stored, merged); err != nil {
			return fmt.Errorf("checkpoint %d: %v\nstored:\n%sincoming:\n%smerged:\n%s", index+1, err, stored, incoming, merged)
		}
		stored = merged
	}
	staged := lvTerminalStaged(t, dir, lvAgentRaw(prefix, raw, scenario.prefix, scenario.n), scenario.policy)
	merged, err := mergeHostAgentTerminalEvents(stored, staged)
	if err != nil {
		return fmt.Errorf("HonestAccepted: terminal transfer rejected: %v\nstored:\n%sstaged:\n%s", err, stored, staged)
	}
	lvNote(stored, merged)
	lvCoverage["terminal transfer merged"]++
	if err := lvCheckTransition(stored, merged); err != nil {
		return fmt.Errorf("terminal: %v\nstored:\n%sstaged:\n%smerged:\n%s", err, stored, staged, merged)
	}
	return nil
}

// Reachable pairs only: hostAgentProgressLedgerPolicy caps the Run Ledger
// policy, so progressEvents <= terminalEvents (equal below 10,000 events).
var lvLedgerPolicies = []lvLedgerPolicyCase{
	{progressEvents: 3, terminalEvents: 3, maxBytes: 1 << 20},
	{progressEvents: 4, terminalEvents: 4, maxBytes: 1 << 20},
	{progressEvents: 3, terminalEvents: 5, maxBytes: 1 << 20},
	{progressEvents: 4, terminalEvents: 6, maxBytes: 1 << 20},
	{progressEvents: 6, terminalEvents: 6, maxBytes: 1 << 20},
	// byte-bound eviction and oversized-event replacement (maxLineBytes = 600)
	{progressEvents: 6, terminalEvents: 6, maxBytes: 1200},
}

// TestLedgerVerifyHonestAgent: every honest Agent run up to the bound keeps
// event identities through every checkpoint and the terminal transfer.
func TestLedgerVerifyHonestAgent(t *testing.T) {
	maxEvents := 9
	maxCheckpoints := 3
	if value := os.Getenv("LEDGER_VERIFY_MAX_EVENTS"); value != "" {
		fmt.Sscan(value, &maxEvents)
	}
	dir := t.TempDir()
	scenarios := 0
	var failures []string
	for _, policy := range lvLedgerPolicies {
		bigSets := []map[int]bool{{}}
		if policy.maxBytes < 1<<20 {
			bigSets = append(bigSets, map[int]bool{1: true}, map[int]bool{3: true}, map[int]bool{2: true, 5: true})
		}
		for n := 1; n <= maxEvents; n++ {
			for prefix := 1; prefix <= lvMaxPrefix() && prefix <= n; prefix++ {
				for length := 0; length <= maxCheckpoints; length++ {
					for _, checkpoints := range lvNondecreasing(prefix, n, length) {
						for _, big := range bigSets {
							scenario := lvLedgerScenario{n: n, prefix: prefix, checkpoints: checkpoints, policy: policy, big: big}
							scenarios++
							if err := lvReplay(t, dir, scenario); err != nil {
								failures = append(failures, scenario.String()+"\n"+err.Error())
							}
						}
					}
				}
			}
		}
	}
	t.Logf("honest scenarios checked: %d", scenarios)
	for _, canary := range []string{
		"stored stream has a retention gap", "gap widened by a later snapshot",
		"acknowledged event absorbed into a gap", "oversized event replaced",
		"new events appended", "terminal transfer merged",
	} {
		if lvCoverage[canary] == 0 {
			t.Errorf("canary never reached: %s", canary)
		} else {
			t.Logf("canary reached %d times: %s", lvCoverage[canary], canary)
		}
	}
	lvReportLedgerFailures(t, failures)
}

func lvMaxPrefix() int {
	prefix := 3
	if value := os.Getenv("LEDGER_VERIFY_MAX_PREFIX"); value != "" {
		fmt.Sscan(value, &prefix)
	}
	return prefix
}

func lvReportLedgerFailures(t *testing.T, failures []string) {
	t.Helper()
	if len(failures) == 0 {
		return
	}
	// Shortest counterexample first; it is the one to read.
	sort.Slice(failures, func(i, j int) bool { return len(failures[i]) < len(failures[j]) })
	kinds := map[string]int{}
	for _, failure := range failures {
		for _, name := range []string{"HonestAccepted", "WellFormed", "Monotone", "IdentityStable", "NoRestore"} {
			if strings.Contains(failure, name+":") {
				kinds[name]++
				break
			}
		}
	}
	t.Errorf("%d scenarios failed %v; shortest:\n%s", len(failures), kinds, failures[0])
}

// lvAdversarialIncoming derives hostile or buggy checkpoints from an honest one.
func lvAdversarialIncoming(honest []byte) [][]byte {
	lines := bytes.Split(bytes.TrimSpace(honest), []byte{'\n'})
	var out [][]byte
	add := func(candidate [][]byte) { out = append(out, lvJoinLines(candidate)) }
	for i := range lines {
		dropped := append(append([][]byte{}, lines[:i]...), lines[i+1:]...)
		add(dropped)
		duplicated := append(append([][]byte{}, lines[:i+1]...), lines[i:]...)
		add(duplicated)
		for _, edit := range []func(map[string]any){
			func(event map[string]any) { event["details"] = map[string]any{"message": "rewritten"} },
			func(event map[string]any) { event["timestamp"] = "1999-01-01T00:00:00Z" },
			func(event map[string]any) {
				event["sequence"] = json.Number(fmt.Sprint(lvMustInt(event["sequence"]) + 1))
			},
			func(event map[string]any) {
				if details, ok := event["details"].(map[string]any); ok && event["type"] == lvGapType {
					count := lvMustInt(details["omittedCount"])
					details["omittedCount"] = count - 1
					details["lastOmittedSequence"] = lvMustInt(details["lastOmittedSequence"]) - 1
				}
			},
			func(event map[string]any) {
				if details, ok := event["details"].(map[string]any); ok && event["type"] == lvGapType {
					count := lvMustInt(details["omittedCount"])
					details["omittedCount"] = count + 1
					details["lastOmittedSequence"] = lvMustInt(details["lastOmittedSequence"]) + 1
				}
			},
		} {
			decoder := json.NewDecoder(bytes.NewReader(lines[i]))
			decoder.UseNumber()
			var event map[string]any
			if decoder.Decode(&event) != nil {
				continue
			}
			edit(event)
			encoded, _ := json.Marshal(event)
			changed := append([][]byte{}, lines...)
			changed[i] = encoded
			add(changed)
		}
	}
	if len(lines) > 1 {
		add(lines[:len(lines)-1])
	}
	return out
}

func lvMustInt(value any) int {
	switch typed := value.(type) {
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	case float64:
		return int(typed)
	case int:
		return typed
	}
	return 0
}

// TestLedgerVerifyAdversarialCheckpoint: whatever an authenticated Agent
// sends, a checkpoint the Control Plane accepts never breaks the stored stream.
func TestLedgerVerifyAdversarialCheckpoint(t *testing.T) {
	dir := t.TempDir()
	checked := 0
	var failures []string
	for _, policy := range lvLedgerPolicies[:5] {
		for n := 2; n <= 7; n++ {
			raw := lvRawEvents(n, nil)
			for storedK := 1; storedK <= n; storedK++ {
				stored := lvAgentCheckpoint(t, dir, raw[:storedK], policy)
				for incomingK := storedK; incomingK <= n; incomingK++ {
					honest := lvAgentCheckpoint(t, dir, raw[:incomingK], policy)
					candidates := lvAdversarialIncoming(honest)
					// An Agent bounding with a different policy (config drift or a
					// hostile Agent) sends narrower or wider gaps for the same run.
					for _, other := range lvLedgerPolicies {
						candidates = append(candidates, lvAgentCheckpoint(t, dir, raw[:incomingK], other))
					}
					for _, incoming := range candidates {
						for _, merge := range []struct {
							name string
							fn   func([]byte, []byte) ([]byte, error)
						}{{"progress", mergeHostAgentProgressEvents}, {"terminal", mergeHostAgentTerminalEvents}} {
							checked++
							merged, err := merge.fn(stored, incoming)
							if err != nil {
								continue
							}
							if err := lvCheckTransition(stored, merged); err != nil {
								failures = append(failures, fmt.Sprintf("%s merge, policy=%+v\n%v\nstored:\n%sincoming:\n%smerged:\n%s", merge.name, policy, err, stored, incoming, merged))
							}
						}
					}
				}
			}
		}
	}
	t.Logf("adversarial merges checked: %d", checked)
	lvReportLedgerFailures(t, failures)
}

// FuzzLedgerVerifyMerge explores arbitrary incoming bytes beyond the
// enumerated mutations. check.sh runs it only with --fuzz.
func FuzzLedgerVerifyMerge(f *testing.F) {
	policy := lvLedgerPolicies[0]
	raw := lvRawEvents(6, nil)
	stored := make([][]byte, 0, 6)
	for k := 1; k <= 6; k++ {
		stored = append(stored, lvAgentCheckpoint(&testing.T{}, f.TempDir(), raw[:k], policy))
		f.Add(uint8(k), stored[k-1], false)
	}
	f.Fuzz(func(t *testing.T, storedK uint8, incoming []byte, terminal bool) {
		current := stored[int(storedK%6)]
		merge := mergeHostAgentProgressEvents
		if terminal {
			merge = mergeHostAgentTerminalEvents
		}
		merged, err := merge(current, incoming)
		if err != nil {
			return
		}
		if err := lvCheckTransition(current, merged); err != nil {
			t.Fatalf("%v\nstored:\n%sincoming:\n%smerged:\n%s", err, current, incoming, merged)
		}
	})
}

// TestLedgerVerifyKeyCaseIdentity: readers take an event's identity from the
// exact "sequence" and "type" keys (ledgerEventSequence, readControlPlaneEvents).
// A checkpoint whose identity keys differ only in letter case, or carry a
// second case-variant key, must not be stored. Known finding at fc665f9: the
// merge decodes identity with case-insensitive struct tags and accepts these.
func TestLedgerVerifyKeyCaseIdentity(t *testing.T) {
	dir := t.TempDir()
	policy := lvLedgerPolicies[0]
	raw := lvRawEvents(5, nil)
	stored := lvAgentCheckpoint(t, dir, raw[:3], policy)
	honest := bytes.Split(bytes.TrimSpace(lvAgentCheckpoint(t, dir, raw[:4], policy)), []byte{'\n'})
	last := honest[len(honest)-1]
	variants := map[string][]byte{
		"case-only sequence key": bytes.Replace(last, []byte(`"sequence":`), []byte(`"sEquence":`), 1),
		"case-only type key":     bytes.Replace(last, []byte(`"type":`), []byte(`"tYpe":`), 1),
		// struct decoding keeps the last matching key (4); readers keep "sequence" (2)
		"duplicate case-variant sequence key": bytes.Replace(last, []byte(`"sequence":4`), []byte(`"sequence":2,"Sequence":4`), 1),
	}
	var failures []string
	for name, line := range variants {
		incoming := lvJoinLines(append(append([][]byte{}, honest[:len(honest)-1]...), line))
		for _, merge := range []struct {
			name string
			fn   func([]byte, []byte) ([]byte, error)
		}{{"progress", mergeHostAgentProgressEvents}, {"terminal", mergeHostAgentTerminalEvents}} {
			merged, err := merge.fn(stored, incoming)
			if err != nil {
				continue
			}
			if err := lvCheckTransition(stored, merged); err != nil {
				failures = append(failures, fmt.Sprintf("%s merge, %s\n%v\nincoming line: %s\nmerged:\n%s", merge.name, name, err, line, merged))
			}
		}
	}
	lvReportLedgerFailures(t, failures)
}
