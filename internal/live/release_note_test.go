package live_test

import (
	"errors"
	"testing"

	"localstream/internal/live"
	"localstream/internal/media"
	"localstream/internal/player"
	"localstream/internal/store"
)

func TestCorrectionReleaseNotesContinuousComparisonWithdrawalAndReopen(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 8)
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.PublishNext()
	if err != nil {
		t.Fatal(err)
	}
	original, err := publisher.PublishNext()
	if err != nil {
		t.Fatal(err)
	}

	viewer, _, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	if err := viewer.JumpTo(media.SegmentDuration); err != nil {
		t.Fatal(err)
	}

	first, err := publisher.Correct(1, "wrong lower third")
	if err != nil {
		t.Fatal(err)
	}
	firstNote, err := publisher.ReleaseNote(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstNote.Kind != store.ReleaseCorrect || firstNote.ParentID != original.ID || firstNote.Reason != "wrong lower third" || firstNote.Correction != 1 || len(firstNote.Changes) != len(media.Variants) {
		t.Fatalf("unexpected first correction note: %+v", firstNote)
	}
	originalSums := checksumsForSegment(t, original, 1)
	firstSums := checksumsForSegment(t, first, 1)
	for _, change := range firstNote.Changes {
		if change.BeforeSHA256 != originalSums[change.Variant] || change.AfterSHA256 != firstSums[change.Variant] || change.AdoptedFrom != first.ID {
			t.Fatalf("unexpected correction record: %+v", change)
		}
	}

	second, err := publisher.Correct(1, "color grade update")
	if err != nil {
		t.Fatal(err)
	}
	secondNote, err := publisher.ReleaseNote(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondNote.ParentID != first.ID || secondNote.Correction != 2 || secondNote.Reason != "color grade update" {
		t.Fatalf("unexpected second correction note: %+v", secondNote)
	}
	secondSums := checksumsForSegment(t, second, 1)
	for _, change := range secondNote.Changes {
		if change.BeforeSHA256 != firstSums[change.Variant] || change.AfterSHA256 != secondSums[change.Variant] {
			t.Fatalf("unexpected continuous correction record: %+v", change)
		}
	}

	originalToSecond, err := publisher.CompareReleases(original.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(originalToSecond.Ranges) != 1 || originalToSecond.Ranges[0].Start != media.SegmentDuration {
		t.Fatalf("original to second ranges=%+v", originalToSecond.Ranges)
	}
	firstToSecond, err := publisher.CompareReleases(first.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstToSecond.Segments) != 2 {
		t.Fatalf("comparison segments=%d", len(firstToSecond.Segments))
	}
	for _, segment := range firstToSecond.Segments {
		if segment.Segment != 1 {
			continue
		}
		for _, variant := range segment.Variants {
			wantChanged := variant.FromSHA256 != variant.ToSHA256
			if variant.Changed != wantChanged || variant.FromSHA256 != firstSums[variant.Name] || variant.ToSHA256 != secondSums[variant.Name] {
				t.Fatalf("unexpected variant comparison: %+v", variant)
			}
		}
	}

	if _, _, err := viewer.AdoptLatest(); err != nil {
		t.Fatal(err)
	}
	decision, err := viewer.CurrentDecision()
	if err != nil {
		t.Fatal(err)
	}
	if decision.ID != second.ID {
		t.Fatalf("viewer decision=%s, want %s", decision.ID, second.ID)
	}
	for _, variant := range media.Variants {
		origin, content, err := viewer.AdoptedReleaseID(variant.Name, 1)
		if err != nil {
			t.Fatal(err)
		}
		if origin != second.ID || content.AfterSHA256 != secondSums[variant.Name] {
			t.Fatalf("origin=%s content=%+v for %s", origin, content, variant.Name)
		}
	}

	withdrawn, err := publisher.Withdraw(second.ID, "correction rejected by compliance")
	if err != nil {
		t.Fatal(err)
	}
	withdrawnNote, err := publisher.ReleaseNote(withdrawn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawnNote.Kind != store.ReleaseWithdraw || withdrawnNote.WithdrawnID != second.ID || withdrawnNote.ParentID != second.ID || withdrawnNote.Reason != "correction rejected by compliance" {
		t.Fatalf("unexpected withdrawal note: %+v", withdrawnNote)
	}
	withdrawnSums := checksumsForSegment(t, withdrawn, 1)
	for _, change := range withdrawnNote.Changes {
		if change.BeforeSHA256 != secondSums[change.Variant] || change.AfterSHA256 != firstSums[change.Variant] || change.AdoptedFrom != first.ID {
			t.Fatalf("unexpected withdrawal record: %+v", change)
		}
		if withdrawnSums[change.Variant] != firstSums[change.Variant] {
			t.Fatalf("withdrawn checksum=%s want first correction=%s", withdrawnSums[change.Variant], firstSums[change.Variant])
		}
	}
	if !store.Open(root).VersionExists(first.ID) || !store.Open(root).VersionExists(second.ID) {
		t.Fatal("withdrawal modified or removed an old immutable release")
	}
	oldSecond, err := publisher.ReleaseNote(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if viewer.Publication() != second.ID {
		t.Fatalf("open player moved from %s to %s", second.ID, viewer.Publication())
	}
	if _, _, err := viewer.AdoptLatest(); err != nil {
		t.Fatal(err)
	}
	for _, spec := range media.Variants {
		origin, _, err := viewer.AdoptedReleaseID(spec.Name, 1)
		if err != nil {
			t.Fatal(err)
		}
		if origin != first.ID {
			t.Fatalf("withdrawn segment origin=%s, want %s", origin, first.ID)
		}
	}
	if _, err := publisher.Withdraw(second.ID, "already withdrawn"); err == nil {
		t.Fatal("second withdrawal of an inactive correction succeeded")
	}
	restored, err := publisher.WithdrawCorrection(1, "all correction decisions rejected")
	if err != nil {
		t.Fatal(err)
	}
	restoredNote, err := publisher.ReleaseNote(restored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restoredNote.WithdrawnID != first.ID || restoredNote.ParentID != withdrawn.ID {
		t.Fatalf("unexpected restore note: %+v", restoredNote)
	}
	frozenSecond, err := publisher.ReleaseNote(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range oldSecond.Changes {
		if frozenSecond.Changes[i].AfterSHA256 != oldSecond.Changes[i].AfterSHA256 {
			t.Fatalf("old correction note changed after withdrawal: %+v -> %+v", oldSecond.Changes[i], frozenSecond.Changes[i])
		}
	}
	restoredSums := checksumsForSegment(t, restored, 1)
	for _, change := range restoredNote.Changes {
		if change.AfterSHA256 != originalSums[change.Variant] || change.AdoptedFrom != original.ID {
			t.Fatalf("unexpected original restoration: %+v", change)
		}
		if restoredSums[change.Variant] != originalSums[change.Variant] {
			t.Fatalf("restored checksum mismatch for %s", change.Variant)
		}
	}

	reopened, err := live.NewPublisher(root, "news", 8)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := reopened.ReleaseNotes()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 6 || notes[len(notes)-1].ID != restored.ID {
		t.Fatalf("release history after reopen=%d last=%+v", len(notes), notes)
	}
	currentNote, err := reopened.CurrentReleaseNote()
	if err != nil {
		t.Fatal(err)
	}
	if currentNote.ID != restored.ID {
		t.Fatalf("current note after reopen=%s", restored.ID)
	}
	comparisonAfterReopen, err := reopened.CompareReleases(first.ID, withdrawn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(comparisonAfterReopen.Ranges) != 0 {
		t.Fatalf("first correction to withdrawal ranges=%+v", comparisonAfterReopen.Ranges)
	}
}

func TestReleaseDecisionsSurviveMediaReclamationAfterPlayerRelease(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishNext(); err != nil {
		t.Fatal(err)
	}
	original, err := publisher.PublishNext()
	if err != nil {
		t.Fatal(err)
	}

	viewer, current, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != original.ID {
		t.Fatalf("opened %s, want %s", current.ID, original.ID)
	}
	if _, _, err := viewer.ReadSegment(); err != nil {
		t.Fatal(err)
	}

	first, err := publisher.Correct(1, "first correction")
	if err != nil {
		t.Fatal(err)
	}
	second, err := publisher.Correct(1, "second correction")
	if err != nil {
		t.Fatal(err)
	}
	withdrawn, err := publisher.Withdraw(second.ID, "second correction withdrawn")
	if err != nil {
		t.Fatal(err)
	}
	if viewer.Publication() != original.ID {
		t.Fatalf("historical query changed player adoption to %s", viewer.Publication())
	}
	origin, _, err := viewer.AdoptedReleaseID("base", 1)
	if err != nil {
		t.Fatal(err)
	}
	if origin != original.ID {
		t.Fatalf("old player origin=%s, want %s", origin, original.ID)
	}

	for range 3 {
		if _, err := publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := viewer.AdoptLatest(); !errors.Is(err, player.ErrWindowExpired) {
		t.Fatalf("adopt after expiry error=%v, want ErrWindowExpired", err)
	}
	if viewer.Publication() != original.ID {
		t.Fatalf("expired adoption moved player to %s", viewer.Publication())
	}
	if _, _, err := viewer.ReadSegment(); err != nil {
		t.Fatalf("retained old media is unreadable before release: %v", err)
	}
	s := store.Open(root)
	if !s.VersionExists(original.ID) {
		t.Fatal("player-held media was reclaimed before Close")
	}
	if s.VersionExists(first.ID) || s.VersionExists(second.ID) || s.VersionExists(withdrawn.ID) {
		t.Fatal("unreferenced correction media avoided reclamation after window expiry")
	}
	if err := viewer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishNext(); err != nil {
		t.Fatal(err)
	}
	if s.VersionExists(original.ID) {
		t.Fatal("old media remained after the final reference was released")
	}

	for _, id := range []string{original.ID, first.ID, second.ID, withdrawn.ID} {
		note, err := publisher.ReleaseNote(id)
		if err != nil {
			t.Fatalf("decision %s lost with media: %v", id, err)
		}
		if note.ID != id {
			t.Fatalf("decision id=%s, want %s", note.ID, id)
		}
	}
	secondNote, err := publisher.ReleaseNote(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondNote.ParentID != first.ID || secondNote.WithdrawnID != "" || secondNote.Reason != "second correction" {
		t.Fatalf("second correction history incomplete: %+v", secondNote)
	}
	withdrawnNote, err := publisher.ReleaseNote(withdrawn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawnNote.ParentID != second.ID || withdrawnNote.WithdrawnID != second.ID {
		t.Fatalf("withdrawal lineage incomplete: %+v", withdrawnNote)
	}
	comparison, err := publisher.CompareReleases(original.ID, second.ID)
	if err != nil {
		t.Fatalf("historical comparison after media reclamation failed: %v", err)
	}
	if len(comparison.Ranges) != 1 || comparison.Ranges[0].Start != media.SegmentDuration {
		t.Fatalf("unexpected historical ranges: %+v", comparison.Ranges)
	}
	withdrawnComparison, err := publisher.CompareReleases(second.ID, withdrawn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(withdrawnComparison.Ranges) != 1 || len(withdrawnComparison.Segments) != 2 {
		t.Fatalf("withdrawal comparison unavailable from decisions: %+v", withdrawnComparison)
	}

	reopened, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := reopened.ReleaseNotes()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 9 || notes[len(notes)-1].ID != reopened.VersionID(8) {
		t.Fatalf("reopened history len=%d notes=%+v", len(notes), notes)
	}
	reopenedComparison, err := reopened.CompareReleases(first.ID, withdrawn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopenedComparison.Ranges) != 0 {
		t.Fatalf("withdrawn second correction should equal first: %+v", reopenedComparison.Ranges)
	}
	newViewer, currentAfterReopen, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	defer newViewer.Close()
	if currentAfterReopen.ID != reopened.VersionID(8) {
		t.Fatalf("current after reopen=%s", currentAfterReopen.ID)
	}
	currentDecision, err := newViewer.CurrentDecision()
	if err != nil {
		t.Fatal(err)
	}
	if currentDecision.ID != currentAfterReopen.ID {
		t.Fatalf("current decision=%s, publication=%s", currentDecision.ID, currentAfterReopen.ID)
	}
}

func checksumsForSegment(t *testing.T, index *store.Index, segmentIndex int) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, variant := range index.Variants {
		for _, segment := range variant.Segments {
			if segment.Index == segmentIndex {
				result[variant.Name] = segment.SHA256
			}
		}
	}
	if len(result) != len(media.Variants) {
		t.Fatalf("index %s missing checksums for segment %d", index.ID, segmentIndex)
	}
	return result
}
