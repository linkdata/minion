package action

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
)

func TestBundledAuditValidatorCoverageRules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bundled audit validators require OS no-follow and nonblocking file flags")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("audit validator requires node")
	}
	script := filepath.Join("..", "skills", "bundled", "security-audit", "validate-coverage-ledger.cjs")
	missing := strings.ReplaceAll(auditCoveredLedgerFixture, "ATTACK-CLASSES.md#Access control", "ATTACK-CLASSES.md#Imaginary")
	missing = strings.Replace(missing, "ATTACK-CLASSES.md%23Access%20control", "ATTACK-CLASSES.md%23Imaginary", 1)
	companionClass := strings.ReplaceAll(auditCoveredLedgerFixture, "ATTACK-CLASSES.md#Access control", "WEB-PROTOCOL-AND-AUTH.md#Ordinary CSRF")
	companionClass = strings.Replace(companionClass, "ATTACK-CLASSES.md%23Access%20control", "WEB-PROTOCOL-AND-AUTH.md%23Ordinary%20CSRF", 1)
	companionClass = strings.Replace(companionClass, `"ordinary_attack_class_block":"WEB-PROTOCOL-AND-AUTH.md#Ordinary CSRF"`, `"ordinary_attack_class_block":null`, 1)
	companionClass = strings.Replace(companionClass, `"selected_companion_blocks":[]`, `"selected_companion_blocks":["WEB-PROTOCOL-AND-AUTH.md#Ordinary CSRF"]`, 1)
	const companionBlocks = `"selected_companion_blocks":["WEB-PROTOCOL-AND-AUTH.md#Core discipline","WEB-PROTOCOL-AND-AUTH.md#Ordinary CSRF","WEB-PROTOCOL-AND-AUTH.md#Universal moves","WEB-PROTOCOL-AND-AUTH.md#Validation rules"]`
	companionWithContext := strings.Replace(companionClass, `"selected_companion_blocks":["WEB-PROTOCOL-AND-AUTH.md#Ordinary CSRF"]`, companionBlocks, 1)
	ordinaryWithContext := strings.Replace(auditCoveredLedgerFixture, `"selected_companion_blocks":[]`, companionBlocks, 1)
	validExcluded := strings.Replace(auditCoveredLedgerFixture, `"excluded_blocks":[]`, `"excluded_blocks":[{"block":"ATTACK-CLASSES.md#Injection","reason":"Outside this boundary."}]`, 1)
	companionContext := strings.ReplaceAll(companionClass, "Ordinary CSRF", "Core discipline")
	companionContext = strings.ReplaceAll(companionContext, "Ordinary%20CSRF", "Core%20discipline")
	archived := auditArchivedCoveredBlockedLedgerFixture()
	missingCurrentOwner := strings.Replace(auditCoveredLedgerFixture, `,{"agent_id":"hunter","reviewed_paths":["source.txt"],"invariant":"Access is checked","method":"source","result":"The guard is present","artifact":null}`, "", 1)
	missingArchivedOwner := strings.Replace(archived, `,{"agent_id":"older","reviewed_paths":["source.txt"],"invariant":"Access is checked","method":"source","result":"The guard is present","artifact":null}`, "", 1)
	reusedArchivedCheck := strings.Replace(archived, `"agent_id":"verifier"`, `"agent_id":"helper"`, 1)
	for _, test := range []struct {
		name, ledger, wantError string
		valid                   bool
	}{
		{name: "assigned", ledger: auditCoveredLedgerFixture, valid: true},
		{name: "archived assignment", ledger: archived, valid: true},
		{name: "empty ledger", ledger: `[]`, wantError: "at least one coverage unit"},
		{name: "unsafe current wave", ledger: strings.Replace(auditCoveredLedgerFixture, `"wave":1`, `"wave":9007199254740992`, 1), wantError: "positive integer"},
		{name: "unsafe archived wave", ledger: strings.Replace(archived, `"wave":1,"status":"covered","agent_id":"older"`, `"wave":9007199254740992,"status":"covered","agent_id":"older"`, 1), wantError: "positive integer"},
		{name: "current owner has no check", ledger: missingCurrentOwner, wantError: "assigned agent must own a local check"},
		{name: "archived owner has no check", ledger: missingArchivedOwner, wantError: "assigned agent must own a local check"},
		{name: "archived check owner reused", ledger: reusedArchivedCheck, wantError: "prior assignment owner evidence"},
		{name: "ordinary with companion", ledger: ordinaryWithContext, valid: true},
		{name: "valid excluded block", ledger: validExcluded, valid: true},
		{name: "unknown excluded block", ledger: strings.Replace(validExcluded, "ATTACK-CLASSES.md#Injection", "ATTACK-CLASSES.md#Imaginary", 1), wantError: "block is absent"},
		{name: "assigned ordinary excluded", ledger: strings.Replace(validExcluded, "ATTACK-CLASSES.md#Injection", "ATTACK-CLASSES.md#Access control", 1), wantError: "block is also selected"},
		{name: "selected companion excluded", ledger: strings.Replace(ordinaryWithContext, `"excluded_blocks":[]`, `"excluded_blocks":[{"block":"WEB-PROTOCOL-AND-AUTH.md#Ordinary CSRF","reason":"Outside this boundary."}]`, 1), wantError: "block is also selected"},
		{name: "companion-only class", ledger: companionWithContext, valid: true},
		{name: "ordinary with companion without context", ledger: strings.Replace(auditCoveredLedgerFixture, `"selected_companion_blocks":[]`, `"selected_companion_blocks":["WEB-PROTOCOL-AND-AUTH.md#Ordinary CSRF"]`, 1), wantError: "class requires Core discipline"},
		{name: "companion without context", ledger: companionClass, wantError: "class requires Core discipline"},
		{name: "missing core discipline", ledger: strings.Replace(companionWithContext, `"WEB-PROTOCOL-AND-AUTH.md#Core discipline",`, "", 1), wantError: "class requires Core discipline"},
		{name: "missing universal moves", ledger: strings.Replace(companionWithContext, `,"WEB-PROTOCOL-AND-AUTH.md#Universal moves"`, "", 1), wantError: "class requires Universal moves"},
		{name: "missing validation rules", ledger: strings.Replace(companionWithContext, `,"WEB-PROTOCOL-AND-AUTH.md#Validation rules"`, "", 1), wantError: "class requires Validation rules"},
		{name: "wrong companion context", ledger: strings.Replace(companionWithContext, "WEB-PROTOCOL-AND-AUTH.md#Core discipline", "CLIENT-SIDE.md#Core discipline", 1), wantError: "class requires Core discipline"},
		{name: "companion context selection", ledger: strings.Replace(auditCoveredLedgerFixture, `"selected_companion_blocks":[]`, `"selected_companion_blocks":["WEB-PROTOCOL-AND-AUTH.md#Core discipline"]`, 1), valid: true},
		{name: "companion context as class", ledger: companionContext},
		{name: "noncompanion skill page", ledger: strings.Replace(auditCoveredLedgerFixture, `"selected_companion_blocks":[]`, `"selected_companion_blocks":["SKILL.md#Core principles"]`, 1)},
		{name: "wrong block", ledger: strings.Replace(auditCoveredLedgerFixture, `"ordinary_attack_class_block":"ATTACK-CLASSES.md#Access control"`, `"ordinary_attack_class_block":"ATTACK-CLASSES.md#Injection"`, 1)},
		{name: "no block", ledger: strings.Replace(auditCoveredLedgerFixture, `"ordinary_attack_class_block":"ATTACK-CLASSES.md#Access control"`, `"ordinary_attack_class_block":null`, 1)},
		{name: "absent block", ledger: missing},
	} {
		t.Run(test.name, func(t *testing.T) {
			ledger := filepath.Join(t.TempDir(), "coverage-ledger.json")
			if err := os.WriteFile(ledger, []byte(test.ledger), 0600); err != nil {
				t.Fatal(err)
			}
			output, err := exec.CommandContext(t.Context(), node, script, ledger).CombinedOutput()
			if (err == nil) != test.valid {
				t.Fatalf("validator error = %v; output = %s", err, output)
			}
			if test.wantError != "" && !strings.Contains(string(output), test.wantError) {
				t.Fatalf("validator output = %s; want %q", output, test.wantError)
			}
		})
	}
}

func TestBundledAuditValidatorRejectsUnsafeLineNumbers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bundled audit validators require OS no-follow and nonblocking file flags")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("audit validator requires node")
	}
	script := filepath.Join("..", "skills", "bundled", "security-audit", "validate-findings.cjs")
	for _, test := range []struct{ name, old string }{
		{"trace", `"trace":[{"kind":"entrypoint","file":"source.txt","line":1`},
		{"evidence", `"evidence":[{"file":"source.txt","line":1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			findings := strings.Replace(auditConfirmedFindingFixture, test.old, strings.Replace(test.old, `"line":1`, `"line":9007199254740992`, 1), 1)
			file := filepath.Join(t.TempDir(), "findings.json")
			if err := os.WriteFile(file, []byte(findings), 0600); err != nil {
				t.Fatal(err)
			}
			output, err := exec.CommandContext(t.Context(), node, script, file).CombinedOutput()
			if err == nil || !strings.Contains(string(output), "must be a positive integer") {
				t.Fatalf("unsafe %s line accepted: %v, %s", test.name, err, output)
			}
		})
	}
}

func TestBundledAuditValidatorsRejectSymlinkInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bundled audit validators require OS no-follow and nonblocking file flags")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("audit validator requires node")
	}
	for _, name := range []string{"validate-findings.cjs", "validate-coverage-ledger.cjs"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "audit.json")
			if err := os.WriteFile(file, []byte(`[]`), 0600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link.json")
			if err := os.Symlink(file, link); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join("..", "skills", "bundled", "security-audit", name)
			output, err := exec.CommandContext(t.Context(), node, script, link).CombinedOutput()
			if err == nil || !strings.Contains(string(output), "input must not be a symlink") {
				t.Fatalf("symlink input accepted: %v, %s", err, output)
			}
		})
	}
}

func TestCompleteAuditResultValidatesCoverage(t *testing.T) {
	validLedger := auditCoveredLedgerFixture
	archived := `{"wave":1,"status":"covered","agent_id":"older","reviewed_paths":["source.txt"],"local_checks":[{"agent_id":"helper","reviewed_paths":["source.txt"],"invariant":"Access is checked","method":"source","result":"The guard is present","artifact":null},{"agent_id":"older","reviewed_paths":["source.txt"],"invariant":"Access is checked","method":"source","result":"The guard is present","artifact":null}],"result_fingerprints":[],"unresolved":[],"reassignment_reason":"Rechecked"}`
	withAttempt := strings.Replace(validLedger, `"attempts":[]`, `"attempts":[`+archived+`]`, 1)
	withAttempt = strings.Replace(withAttempt, `"wave":1,"status":"covered","agent_id":"hunter"`, `"wave":2,"status":"covered","agent_id":"hunter"`, 1)
	withLifecycle := strings.Replace(validLedger, `"coverage_id":"source::boundary::subsystem::ATTACK-CLASSES.md%23Access%20control"`, `"coverage_id":"source::boundary::subsystem::ATTACK-CLASSES.md%23Access%20control::lifecycle"`, 1)
	withLifecycle = strings.Replace(withLifecycle, `"attack_class":"ATTACK-CLASSES.md#Access control"}`, `"attack_class":"ATTACK-CLASSES.md#Access control","lifecycle":"lifecycle"}`, 1)
	withLifecycle = strings.Replace(withLifecycle, `"attack_class":"Attack","starting_paths"`, `"attack_class":"Attack","lifecycle":"Lifecycle","starting_paths"`, 1)
	archivedCandidate := strings.Replace(archived, `"status":"covered"`, `"status":"candidate"`, 1)
	archivedCandidate = strings.Replace(archivedCandidate, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1)
	archivedCandidate = strings.Replace(archivedCandidate, `"local_checks":[{"agent_id":"helper"`, `"local_checks":[{"agent_id":"older"`, 1)
	withSelfCheckedAttempt := strings.Replace(validLedger, `"attempts":[]`, `"attempts":[`+archivedCandidate+`]`, 1)
	withSelfCheckedAttempt = strings.Replace(withSelfCheckedAttempt, `"wave":1,"status":"covered","agent_id":"hunter"`, `"wave":2,"status":"covered","agent_id":"hunter"`, 1)
	sourceCheck := `"method":"source","result":"The guard is present","artifact":null`
	localCheck := `"method":"local","result":"The guard is present","artifact":"agents/hunter/artifacts/proof.txt"`
	archivedLocalCheck := `"method":"local","result":"The guard is present","artifact":"agents/helper/artifacts/proof.txt"`
	confirmedLedger := strings.Replace(validLedger, `"status":"covered"`, `"status":"candidate"`, 1)
	confirmedLedger = strings.Replace(confirmedLedger, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1)
	secondCandidate := strings.Replace(confirmedLedger, `"coverage_id":"source::boundary::`, `"coverage_id":"source::second::`, 1)
	secondCandidate = strings.Replace(secondCandidate, `"boundary":"boundary"`, `"boundary":"second"`, 1)
	secondCandidate = strings.Replace(secondCandidate, `"boundary":"Boundary"`, `"boundary":"Secondary boundary"`, 1)
	twoBoundaries := "[" + strings.Trim(confirmedLedger, "[]") + "," + strings.Trim(secondCandidate, "[]") + "]"
	selfCheckedSecond := strings.Replace(secondCandidate, `"local_checks":[{"agent_id":"verifier"`, `"local_checks":[{"agent_id":"hunter"`, 1)
	oneUnverifiedBoundary := "[" + strings.Trim(confirmedLedger, "[]") + "," + strings.Trim(selfCheckedSecond, "[]") + "]"
	blockedLedger := strings.Replace(validLedger, `"status":"covered"`, `"status":"blocked"`, 1)
	blockedLedger = strings.Replace(blockedLedger, `"unresolved":[]}]`, `"unresolved":["Investigate boundary"]}]`, 1)
	excludedLedger := auditOutOfScopeLedgerFixture()
	secondBlocked := strings.Replace(blockedLedger, `"coverage_id":"source::boundary::`, `"coverage_id":"source::blocked::`, 1)
	secondBlocked = strings.Replace(secondBlocked, `"boundary":"boundary"`, `"boundary":"blocked"`, 1)
	secondBlocked = strings.Replace(secondBlocked, `"boundary":"Boundary"`, `"boundary":"Blocked boundary"`, 1)
	blockedAndCovered := "[" + strings.Trim(secondBlocked, "[]") + "," + strings.Trim(validLedger, "[]") + "]"
	ownerCheck := `,{"agent_id":"hunter","reviewed_paths":["source.txt"],"invariant":"Access is checked","method":"source","result":"The guard is present","artifact":null}`
	verifierOnly := strings.Replace(validLedger, ownerCheck, "", 1)
	blockedVerifierOnly := strings.Replace(blockedLedger, ownerCheck, "", 1)
	budgetSymbolInProse := strings.Replace(blockedAndCovered, `"unresolved":["Investigate boundary"]`, `"unresolved":["Investigate source symbol validation_budget_exhausted"]`, 1)
	incompleteBlocked := strings.Replace(blockedAndCovered, `"unresolved":["Investigate boundary"]`, `"unresolved":["validation_budget_exhausted"]`, 1)
	incompleteReservedBudget := strings.Replace(blockedAndCovered, `"unresolved":["Investigate boundary"]`, `"unresolved":["budget_cannot_reserve_critics_and_validation"]`, 1)
	incompleteExcluded := strings.Replace(excludedLedger, `"unresolved":["Out of scope"]`, `"unresolved":["critic_budget_exhausted"]`, 1)
	incompleteExcluded = strings.Replace(incompleteExcluded, `"coverage_id":"source::boundary::`, `"coverage_id":"source::excluded::`, 1)
	incompleteExcluded = strings.Replace(incompleteExcluded, `"boundary":"boundary"`, `"boundary":"excluded"`, 1)
	incompleteExcluded = "[" + strings.Trim(incompleteExcluded, "[]") + "," + strings.Trim(validLedger, "[]") + "]"
	archivedBlocked := strings.Replace(archived, `"status":"covered"`, `"status":"blocked"`, 1)
	archivedBlocked = strings.Replace(archivedBlocked, `"unresolved":[]`, `"unresolved":["budget_cannot_fund_reconnaissance_and_reserves"]`, 1)
	incompleteArchive := strings.Replace(validLedger, `"attempts":[]`, `"attempts":[`+archivedBlocked+`]`, 1)
	incompleteArchive = strings.Replace(incompleteArchive, `"wave":1,"status":"covered","agent_id":"hunter"`, `"wave":2,"status":"covered","agent_id":"hunter"`, 1)
	selfCheckedCandidate := strings.Replace(confirmedLedger, `"local_checks":[{"agent_id":"verifier"`, `"local_checks":[{"agent_id":"hunter"`, 1)
	rejectedFinding := `[{"verdict":"rejected","fingerprint":"F","title":"Guard bypass refuted","description":"The alleged bypass does not reach the operation.","claimed_root_cause":"The guard was claimed to be absent.","trace":[{"kind":"entrypoint","file":"source.txt","line":1,"scope":"source","description":"The request reaches the guard."}],"evidence":[{"file":"source.txt","line":1,"description":"The guard executes before the operation."}],"reason":"The request is stopped by the guard."}]`
	archivedNotApplicable := auditArchivedCandidateNotApplicableLedgerFixture()
	applicableCandidate := strings.Replace(archivedNotApplicable, `"status":"covered"`, `"status":"candidate"`, 1)
	applicableCandidate = strings.Replace(applicableCandidate, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1)
	for _, test := range []struct {
		name, ledger, findings, want string
	}{
		{"needs validation from excluded archive", archivedNotApplicable, auditCandidateFindingFixture, "without applicable candidate"},
		{"confirmed from excluded archive", archivedNotApplicable, auditConfirmedFindingFixture, "without applicable candidate"},
		{"rejected from excluded archive", archivedNotApplicable, rejectedFinding, ""},
		{"active finding with another applicable candidate", applicableCandidate, auditCandidateFindingFixture, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateAuditCoverage([]byte(test.ledger), []byte(test.findings), []byte("Audit report"), nil, nil)
			if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("validateAuditCoverage() = %v; want %q", err, test.want)
			}
		})
	}
	withContext := strings.Replace(validLedger, `"selected_companion_blocks":[]`, `"selected_companion_blocks":["WEB-PROTOCOL-AND-AUTH.md#Core discipline","WEB-PROTOCOL-AND-AUTH.md#Ordinary CSRF","WEB-PROTOCOL-AND-AUTH.md#Universal moves","WEB-PROTOCOL-AND-AUTH.md#Validation rules"]`, 1)
	withContext = strings.Replace(withContext, `"excluded_blocks":[]`, `"excluded_blocks":[{"block":"ATTACK-CLASSES.md#Injection","reason":"Outside this boundary."}]`, 1)
	for _, test := range []struct {
		name, ledger, findings, report, want string
		companion                            bool
	}{
		{name: "covered"},
		{name: "invisible report", report: "\u200b", want: "no visible content"},
		{name: "code-only private report", report: "```md\nExample\n```"},
		{name: "blocked-only ledger has no reviewed coverage", ledger: blockedLedger, want: "no reviewed coverage"},
		{name: "archived covered attempt cannot satisfy blocked current coverage", ledger: auditArchivedCoveredBlockedLedgerFixture(), want: "no reviewed coverage"},
		{name: "excluded-only ledger cannot be complete", ledger: excludedLedger, want: "out-of-scope unit in complete audit"},
		{name: "covered ledger with current out-of-scope unit is incomplete", ledger: auditCoveredAndOutOfScopeLedgerFixture(), want: "out-of-scope unit in complete audit"},
		{name: "blocked and covered", ledger: blockedAndCovered},
		{name: "covered without owner check", ledger: verifierOnly, want: "lacks assigned owner's local check"},
		{name: "candidate without owner check", ledger: strings.Replace(confirmedLedger, ownerCheck, "", 1), findings: auditCandidateFindingFixture, want: "lacks assigned owner's local check"},
		{name: "blocked without owner check", ledger: blockedVerifierOnly, want: "lacks assigned owner's local check"},
		{name: "archived attempt without owner check", ledger: strings.Replace(withAttempt, `,{"agent_id":"older","reviewed_paths":["source.txt"],"invariant":"Access is checked","method":"source","result":"The guard is present","artifact":null}`, "", 1), want: "lacks assigned owner's local check"},
		{name: "budget code inside unresolved prose", ledger: budgetSymbolInProse},
		{name: "blocked unit declares exhausted validation budget", ledger: incompleteBlocked, want: "incomplete audit budget"},
		{name: "blocked unit cannot reserve critics and validation", ledger: incompleteReservedBudget, want: "incomplete audit budget"},
		{name: "reserved agent ID", ledger: strings.Replace(validLedger, `"agent_id":"hunter"`, `"agent_id":"con"`, 1), want: "lacks assigned review evidence"},
		{name: "excluded unit declares exhausted critic budget", ledger: incompleteExcluded, want: "incomplete audit budget"},
		{name: "archived attempt declares unaffordable budget", ledger: incompleteArchive, want: "incomplete audit budget"},
		{name: "covered with populated context", ledger: withContext, companion: true},
		{name: "malformed excluded block", ledger: strings.Replace(withContext, "ATTACK-CLASSES.md#Injection", "Companion B", 1), want: "invalid excluded block", companion: true},
		{name: "custom excluded block", ledger: strings.Replace(withContext, "ATTACK-CLASSES.md#Injection", "CUSTOM-AUDIT.md#New class", 1), companion: true},
		{name: "assigned ordinary excluded", ledger: strings.Replace(withContext, "ATTACK-CLASSES.md#Injection", "ATTACK-CLASSES.md#Access control", 1), want: "invalid excluded block", companion: true},
		{name: "selected companion excluded", ledger: strings.Replace(withContext, "ATTACK-CLASSES.md#Injection", "WEB-PROTOCOL-AND-AUTH.md#Ordinary CSRF", 1), want: "invalid excluded block", companion: true},
		{name: "covered with archived attempt and distinct check owners", ledger: withAttempt},
		{name: "covered with lifecycle", ledger: withLifecycle},
		{name: "invisible surface label", ledger: strings.Replace(validLedger, `"surface":"Source"`, `"surface":"\u200b"`, 1), want: "lacks unique coverage identity"},
		{name: "invisible boundary label", ledger: strings.Replace(validLedger, `"boundary":"Boundary"`, `"boundary":"\u200b"`, 1), want: "lacks unique coverage identity"},
		{name: "invisible subsystem label", ledger: strings.Replace(validLedger, `"subsystem":"Subsystem"`, `"subsystem":"\u200b"`, 1), want: "lacks unique coverage identity"},
		{name: "invisible attack class label", ledger: strings.Replace(validLedger, `"attack_class":"Attack"`, `"attack_class":"\u200b"`, 1), want: "lacks unique coverage identity"},
		{name: "invisible lifecycle label", ledger: strings.Replace(withLifecycle, `"lifecycle":"Lifecycle"`, `"lifecycle":"\u200b"`, 1), want: "lacks lifecycle identity"},
		{name: "invisible canonical reference", ledger: strings.Replace(validLedger, `"boundary":"boundary"`, `"boundary":"\u200b"`, 1), want: "lacks canonical coverage references"},
		{name: "invisible archived reassignment reason", ledger: strings.Replace(withAttempt, `"reassignment_reason":"Rechecked"`, `"reassignment_reason":"\u200b"`, 1), want: "invalid archived attempt wave or reassignment"},
		{name: "unsupported local check in current evidence", ledger: strings.Replace(validLedger, sourceCheck, localCheck, 1), want: "unsupported local check method"},
		{name: "unsupported local check in archived evidence", ledger: strings.Replace(withAttempt, sourceCheck, archivedLocalCheck, 1), want: "unsupported local check method"},
		{name: "decimal spelling of integer wave", ledger: strings.Replace(validLedger, `"wave":1`, `"wave":1.0`, 1)},
		{name: "exponent spelling of integer wave", ledger: strings.Replace(validLedger, `"wave":1`, `"wave":1e0`, 1)},
		{name: "decimal spelling of archived wave", ledger: strings.Replace(withAttempt, `"wave":1`, `"wave":1.0`, 1)},
		{name: "fractional archived wave", ledger: strings.Replace(withAttempt, `"wave":1`, `"wave":1.5`, 1), want: "invalid archived attempt wave"},
		{name: "fractional current wave", ledger: strings.Replace(validLedger, `"wave":1`, `"wave":1.5`, 1), want: "invalid current wave"},
		{name: "zero current wave", ledger: strings.Replace(validLedger, `"wave":1`, `"wave":0`, 1), want: "invalid current wave"},
		{name: "confirmed finding", ledger: confirmedLedger, findings: auditConfirmedFindingFixture},
		{name: "confirmed claim without source-derived evidence", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"observed_result":"Source-derived: the branch writes the other user's object."`, `"observed_result":"The other user's object changes."`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed finding with second candidate", ledger: twoBoundaries, findings: auditConfirmedFindingFixture},
		{name: "one candidate cannot borrow another candidate's verifier", ledger: oneUnverifiedBoundary, findings: auditConfirmedFindingFixture, want: "without separately attributed source check"},
		{name: "needs_validation finding", ledger: confirmedLedger, findings: auditCandidateFindingFixture},
		{name: "candidate lacks separate check claim", ledger: selfCheckedCandidate, findings: auditCandidateFindingFixture, want: "without separately attributed source check"},
		{name: "archived candidate lacks separate check claim", ledger: withSelfCheckedAttempt, findings: auditCandidateFindingFixture, want: "without separately attributed source check"},
		{name: "decimal spelling of integer line", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"line":1`, `"line":1.0`, 1)},
		{name: "exponent spelling of integer line", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"line":1`, `"line":1e0`, 1)},
		{name: "fractional trace line", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"line":1`, `"line":1.5`, 1), want: "invalid trace evidence"},
		{name: "zero trace line", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"line":1`, `"line":0`, 1), want: "invalid trace evidence"},
		{name: "fractional evidence line", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"evidence":[{"file":"source.txt","line":1`, `"evidence":[{"file":"source.txt","line":1.5`, 1), want: "invalid source evidence"},
		{name: "rejected finding", ledger: confirmedLedger, findings: rejectedFinding},
		{name: "needs_validation without claimed cause", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"claimed_root_cause":"The guard may be absent.",`, ``, 1), want: "incomplete needs_validation finding"},
		{name: "needs_validation without blockers", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"blockers":["Runtime validation is unavailable."],`, ``, 1), want: "incomplete needs_validation finding"},
		{name: "needs_validation without validation plan", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `,"validation_plan":{"local":"Inspect the guard path in an approved test."}`, ``, 1), want: "incomplete needs_validation finding"},
		{name: "needs_validation invisible blocker", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"blockers":["Runtime validation is unavailable."]`, `"blockers":["\u200b"]`, 1), want: "incomplete needs_validation finding"},
		{name: "rejected without claimed cause", ledger: confirmedLedger, findings: strings.Replace(rejectedFinding, `"claimed_root_cause":"The guard was claimed to be absent.",`, ``, 1), want: "incomplete rejected finding"},
		{name: "rejected without reason", ledger: confirmedLedger, findings: strings.Replace(rejectedFinding, `,"reason":"The request is stopped by the guard."`, ``, 1), want: "incomplete rejected finding"},
		{name: "single propagation trace", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"kind":"entrypoint"`, `"kind":"propagation"`, 1), want: "invalid trace evidence"},
		{name: "wrong ordered trace", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"trace":[{"kind":"entrypoint","file":"source.txt","line":1,"scope":"source","description":"Input reaches the boundary."}]`, `"trace":[{"kind":"sink","file":"source.txt","line":1,"scope":"source","description":"The request reaches the sink."},{"kind":"entrypoint","file":"source.txt","line":2,"scope":"source","description":"The request enters later."}]`, 1), want: "invalid trace evidence"},
		{name: "confirmed with typed condition", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"conditions":[]`, `"conditions":[{"kind":"authentication_level","description":"The attacker has an ordinary account."}]`, 1)},
		{name: "confirmed null condition", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"conditions":[]`, `"conditions":[null]`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed unknown condition", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"conditions":[]`, `"conditions":[{"kind":"unknown","description":"The attacker has an ordinary account."}]`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed invisible condition", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"conditions":[]`, `"conditions":[{"kind":"authentication_level","description":"\u200b"}]`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed invisible root cause", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"root_cause":"The operation omits the guard before changing the object."`, `"root_cause":"\u200b"`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed control-only observed result", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"observed_result":"Source-derived: the branch writes the other user's object."`, `"observed_result":"\u0001"`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed missing observed result", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"observed_result":"Source-derived: the branch writes the other user's object."`, `"observed_result":""`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed missing remediation", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"remediation":{"strategy":"Check ownership before the state change."}`, `"remediation":{}`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed missing severity", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"severity":{"likelihood":{"score":"medium","reason":"The operation is directly reachable."},"impact":{"score":"medium","reason":"One protected object can change."},"overall_severity":"medium"},`, ``, 1), want: "incomplete confirmed finding"},
		{name: "empty ledger", ledger: `[]`, want: "no coverage units"},
		{name: "false covered claim", ledger: `[{"status":"covered"}]`, want: "coverage identity"},
		{name: "contradictory duplicate status", ledger: strings.Replace(validLedger, `"status":"covered"`, `"status":"planned","status":"covered"`, 1), want: "duplicate object keys"},
		{name: "sole uppercase status", ledger: strings.Replace(validLedger, `"status":"covered"`, `"STATUS":"covered"`, 1), want: "noncanonical key"},
		{name: "sole uppercase verdict", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"verdict":"confirmed"`, `"VERDICT":"confirmed"`, 1), want: "noncanonical key"},
		{name: "duplicate finding key", findings: `[{"fingerprint":"F","fingerprint":"F"}]`, want: "duplicate object keys"},
		{name: "bare finding", findings: `[{"fingerprint":"F"}]`, want: "incomplete finding"},
		{name: "noncanonical identity", ledger: strings.Replace(validLedger, `"coverage_id":"source::boundary::subsystem::ATTACK-CLASSES.md%23Access%20control"`, `"coverage_id":"forged"`, 1), want: "noncanonical coverage identity"},
		{name: "missing owner", ledger: strings.Replace(validLedger, `"agent_id":"hunter","reviewed_paths"`, `"agent_id":null,"reviewed_paths"`, 1), want: "assigned review evidence"},
		{name: "unchecked path", ledger: strings.Replace(validLedger, `"local_checks":[{"agent_id":"verifier","reviewed_paths":["source.txt"]`, `"local_checks":[{"agent_id":"verifier","reviewed_paths":["other.txt"]`, 1), want: "local check outside reviewed paths"},
		{name: "check without result", ledger: strings.Replace(validLedger, `"result":"The guard is present"`, `"result":""`, 1), want: "incomplete local check"},
		{name: "invisible local check invariant", ledger: strings.Replace(validLedger, `"invariant":"Access is checked"`, `"invariant":"\u200b"`, 1), want: "incomplete local check"},
		{name: "invisible local check result", ledger: strings.Replace(validLedger, `"result":"The guard is present"`, `"result":"\u200b"`, 1), want: "incomplete local check"},
		{name: "invisible unresolved reason", ledger: strings.Replace(blockedAndCovered, `"unresolved":["Investigate boundary"]`, `"unresolved":["\u200b"]`, 1), want: "empty unresolved reason"},
		{name: "covered with unresolved", ledger: strings.Replace(validLedger, `"unresolved":[]}]`, `"unresolved":["ignored"]}]`, 1), want: "inconsistent status evidence"},
		{name: "missing ordinary block", ledger: strings.Replace(validLedger, `"ordinary_attack_class_block":"ATTACK-CLASSES.md#Access control",`, ``, 1), want: "required coverage context"},
		{name: "invalid ordinary block type", ledger: strings.Replace(validLedger, `"ordinary_attack_class_block":"ATTACK-CLASSES.md#Access control"`, `"ordinary_attack_class_block":[]`, 1), want: "invalid ordinary attack class block"},
		{name: "unassigned attack class", ledger: strings.Replace(validLedger, `"ordinary_attack_class_block":"ATTACK-CLASSES.md#Access control"`, `"ordinary_attack_class_block":"ATTACK-CLASSES.md#Injection"`, 1), want: "attack class without an assigned block"},
		{name: "attack class with no block", ledger: strings.Replace(validLedger, `"ordinary_attack_class_block":"ATTACK-CLASSES.md#Access control"`, `"ordinary_attack_class_block":null`, 1), want: "attack class without an assigned block"},
		{name: "missing selected blocks", ledger: strings.Replace(validLedger, `"selected_companion_blocks":[],`, ``, 1), want: "required coverage context"},
		{name: "null selected blocks", ledger: strings.Replace(validLedger, `"selected_companion_blocks":[]`, `"selected_companion_blocks":null`, 1), want: "required coverage context"},
		{name: "missing excluded blocks", ledger: strings.Replace(validLedger, `"excluded_blocks":[],`, ``, 1), want: "required coverage context"},
		{name: "null excluded blocks", ledger: strings.Replace(validLedger, `"excluded_blocks":[]`, `"excluded_blocks":null`, 1), want: "required coverage context"},
		{name: "missing prior status", ledger: strings.Replace(validLedger, `"prior_status":"none",`, ``, 1), want: "required coverage context"},
		{name: "prior run claim without metadata", ledger: strings.Replace(validLedger, `"prior_status":"none"`, `"prior_status":"prior_confirmed_same_source"`, 1), want: "prior run absent from metadata"},
		{name: "new claim without prior ledger", ledger: strings.Replace(validLedger, `"prior_status":"none"`, `"prior_status":"new"`, 1), want: "prior run absent from metadata"},
		{name: "invalid prior status", ledger: strings.Replace(validLedger, `"prior_status":"none"`, `"prior_status":"invented"`, 1), want: "prior run absent from metadata"},
		{name: "missing attempts", ledger: strings.Replace(validLedger, `"attempts":[],`, ``, 1), want: "required coverage context"},
		{name: "null attempts", ledger: strings.Replace(validLedger, `"attempts":[]`, `"attempts":null`, 1), want: "required coverage context"},
		{name: "archived attempt without evidence", ledger: strings.Replace(validLedger, `"attempts":[]`, `"attempts":[{"status":"covered"}]`, 1), want: "assigned review evidence"},
		{name: "archived attempt without wave", ledger: strings.Replace(withAttempt, `"wave":1,"status":"covered"`, `"status":"covered"`, 1), want: "invalid archived attempt wave"},
		{name: "archived attempt without reassignment", ledger: strings.Replace(withAttempt, `,"reassignment_reason":"Rechecked"`, ``, 1), want: "invalid archived attempt wave or reassignment"},
		{name: "reused archived owner", ledger: strings.ReplaceAll(withAttempt, `"agent_id":"older"`, `"agent_id":"hunter"`), want: "reuses an archived assignment owner"},
		{name: "reused archived check owner", ledger: strings.Replace(withAttempt, `"local_checks":[{"agent_id":"verifier"`, `"local_checks":[{"agent_id":"older"`, 1), want: "reuses an archived assignment check owner"},
		{name: "reused archived verifier", ledger: strings.Replace(withAttempt, `"local_checks":[{"agent_id":"verifier"`, `"local_checks":[{"agent_id":"helper"`, 1), want: "reuses an archived assignment check owner"},
		{name: "current wave precedes archive", ledger: strings.Replace(withAttempt, `"wave":2,"status":"covered"`, `"wave":1,"status":"covered"`, 1), want: "invalid archived attempt wave"},
		{name: "null unit", ledger: `[null]`, want: "invalid unit status"},
		{name: "unknown unit status", ledger: `[{"status":"unknown"}]`, want: "invalid unit status"},
		{name: "unfinished unit", ledger: `[{"status":"planned"}]`, want: "unfinished planned unit"},
		{name: "null findings", findings: `null`, want: "not an array"},
		{name: "empty finding fingerprint", findings: `[{"fingerprint":""}]`, want: "empty fingerprint"},
		{name: "unvalidated candidate", ledger: `[{"status":"candidate","result_fingerprints":["F"],"unresolved":["validation_budget_exhausted"]}]`, findings: auditCandidateFindingFixture, want: "incomplete audit budget"},
		{name: "empty candidate fingerprint", ledger: `[{"status":"candidate","result_fingerprints":[""]}]`, want: "candidate without fingerprint"},
		{name: "candidate without finding", ledger: `[{"status":"candidate","result_fingerprints":["F"]}]`, want: "candidate without final finding"},
		{name: "finding without candidate", findings: auditCandidateFindingFixture, want: "finding without candidate"},
		{name: "unvalidated attempt", ledger: `[{"status":"covered","attempts":[{"status":"candidate","result_fingerprints":["F"],"unresolved":["validation_budget_exhausted"]}]}]`, findings: auditCandidateFindingFixture, want: "incomplete audit budget"},
		{name: "unknown attempt status", ledger: `[{"status":"covered","attempts":[{"status":"unknown"}]}]`, want: "invalid attempt status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.SecurityAudit)
			result := completeAuditResult(job)
			if test.findings != "" {
				result.Answer = "Reviewed audit report: Unverified boundary; Missing guard (medium)."
			}
			if strings.Contains(test.findings, `"verdict":"confirmed"`) {
				result.Answer += "\n| Severity | Title | Boundary | Observed result |\n| --- | --- | --- | --- |\n| medium | Missing guard | Boundary | The other user's object changes. |"
			}
			if test.report != "" {
				result.Answer = test.report
			}
			for i := range result.Artifacts {
				switch result.Artifacts[i].Path {
				case "run-metadata.json":
					if test.companion {
						result.Artifacts[i].Data = []byte(strings.Replace(string(result.Artifacts[i].Data), `"selected_companion_files":[]`, `"selected_companion_files":["WEB-PROTOCOL-AND-AUTH.md"]`, 1))
					}
				case "REPORT.md":
					result.Artifacts[i].Data = []byte(result.Answer)
				case "coverage-ledger.json":
					if test.ledger != "" {
						result.Artifacts[i].Data = []byte(test.ledger)
					}
				case "findings.json":
					if test.findings != "" {
						result.Artifacts[i].Data = []byte(test.findings)
					}
				case "FINDINGS-DETAIL.md":
					if strings.Contains(test.findings, `"verdict":"confirmed"`) {
						result.Artifacts[i].Data = []byte(auditConfirmedDetailFixture)
					}
				case "NEEDS-VALIDATION.md":
					if strings.Contains(test.findings, `"verdict":"needs_validation"`) {
						result.Artifacts[i].Data = []byte(auditNeedsDetailFixture)
					}
				}
			}
			err := result.Validate(job)
			if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("complete audit validation = %v, want %q", err, test.want)
			}
			if err := result.validate(job, false); err != nil {
				t.Fatalf("partial audit evidence rejected: %v", err)
			}
		})
	}
}

func TestCompleteAuditCompanionBlocksMatchMetadata(t *testing.T) {
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	for i := range result.Artifacts {
		if result.Artifacts[i].Path == "coverage-ledger.json" {
			result.Artifacts[i].Data = []byte(strings.Replace(string(result.Artifacts[i].Data), `"selected_companion_blocks":[]`, `"selected_companion_blocks":["WEB-PROTOCOL-AND-AUTH.md#Core discipline"]`, 1))
		}
	}
	if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "companion file absent from metadata") {
		t.Fatalf("ledger companion without metadata: %v", err)
	}
	for i := range result.Artifacts {
		if result.Artifacts[i].Path == "run-metadata.json" {
			result.Artifacts[i].Data = []byte(strings.Replace(string(result.Artifacts[i].Data), `"selected_companion_files":[]`, `"selected_companion_files":["WEB-PROTOCOL-AND-AUTH.md"]`, 1))
		}
	}
	if err := result.Validate(job); err != nil {
		t.Fatalf("matching companion metadata rejected: %v", err)
	}
}

func TestAuditVisibleReport(t *testing.T) {
	for _, test := range []struct{ name, report, want string }{
		{name: "prose", report: "Reviewed audit report."},
		{name: "prose after code", report: "```md\nExample\n```\nReviewed audit report."},
		{name: "invisible", report: "\u200b", want: "no visible content"},
		{name: "fenced code only", report: "```md\nExample\n```"},
		{name: "indented code only", report: "    Example"},
		{name: "blockquoted code only", report: "> ```md\n> Example\n> ```"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := auditVisibleReport(test.report)
			if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("report validation = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompleteAuditRequiresArchitectureText(t *testing.T) {
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	for i := range result.Artifacts {
		if result.Artifacts[i].Path == "architecture.md" {
			result.Artifacts[i].Data = []byte(" \t\r\n")
		}
	}
	if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "architecture.md") {
		t.Fatalf("complete audit accepted blank architecture: %v", err)
	}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("partial architecture evidence rejected: %v", err)
	}
}

func TestBundledAuditValidatorsRejectInvalidObjectKeys(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bundled audit validators require OS no-follow and nonblocking file flags")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("audit validator requires node")
	}
	for _, test := range []struct{ name, script, content, want string }{
		{"ledger", "validate-coverage-ledger.cjs", strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"planned","status":"covered"`, 1), "duplicate object keys"},
		{"ledger escaped key", "validate-coverage-ledger.cjs", strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"planned","\u0073tatus":"covered"`, 1), "duplicate object keys"},
		{"ledger non-ASCII key", "validate-coverage-ledger.cjs", strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"planned","ſtatus":"covered"`, 1), "non-ASCII object key"},
		{"ledger escaped non-ASCII key", "validate-coverage-ledger.cjs", strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"planned","\u017ftatus":"covered"`, 1), "non-ASCII object key"},
		{"finding", "validate-findings.cjs", strings.Replace(auditConfirmedFindingFixture, `"verdict":"confirmed"`, `"verdict":"rejected","verdict":"confirmed"`, 1), "duplicate object keys"},
		{"finding escaped key", "validate-findings.cjs", strings.Replace(auditConfirmedFindingFixture, `"verdict":"confirmed"`, `"verdict":"rejected","\u0076erdict":"confirmed"`, 1), "duplicate object keys"},
		{"finding non-ASCII key", "validate-findings.cjs", strings.Replace(auditConfirmedFindingFixture, `"severity":{`, `"severity":{},"ſeverity":{`, 1), "non-ASCII object key"},
		{"finding escaped non-ASCII key", "validate-findings.cjs", strings.Replace(auditConfirmedFindingFixture, `"severity":{`, `"severity":{},"\u017feverity":{`, 1), "non-ASCII object key"},
		{"nested finding", "validate-findings.cjs", strings.Replace(auditConfirmedFindingFixture, `"overall_severity":"medium"`, `"overall_severity":"low","overall_severity":"medium"`, 1), "duplicate object keys"},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "audit.json")
			if err := os.WriteFile(file, []byte(test.content), 0600); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join("..", "skills", "bundled", "security-audit", test.script)
			output, err := exec.CommandContext(t.Context(), node, script, file).CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.want) {
				t.Fatalf("validator accepted invalid key: output=%q error=%v", output, err)
			}
		})
	}
}

func TestAuditJSONRejectsNonASCIIObjectKeys(t *testing.T) {
	for _, source := range []string{
		`{"status":"planned","ſtatus":"covered"}`,
		`{"status":"planned","\u017ftatus":"covered"}`,
	} {
		if err := rejectDuplicateAuditJSONKeys([]byte(source)); err == nil || !strings.Contains(err.Error(), "non-ASCII object key") {
			t.Fatalf("non-ASCII key was accepted: source=%q error=%v", source, err)
		}
	}
	if err := rejectDuplicateAuditJSONKeys([]byte(`{"status":"café"}`)); err != nil {
		t.Fatalf("Unicode value was rejected: %v", err)
	}
}
