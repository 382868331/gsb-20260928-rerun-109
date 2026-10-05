package live_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"localstream/internal/live"
	"localstream/internal/media"
	"localstream/internal/player"
	"localstream/internal/store"
)

func TestReleaseDecisionsSurviveMediaReclaimAfterExpiredReaderReleases(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	var original *store.Index
	for range 4 {
		if original, err = publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}

	viewer, opened, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	if opened.ID != original.ID {
		t.Fatalf("opened %s, want %s", opened.ID, original.ID)
	}

	first, err := publisher.Correct(1, "first correction")
	if err != nil {
		t.Fatal(err)
	}
	second, err := publisher.Correct(1, "second correction")
	if err != nil {
		t.Fatal(err)
	}
	withdrawn, err := publisher.Withdraw(second.ID, "reject second correction")
	if err != nil {
		t.Fatal(err)
	}
	heldID := original.ID
	if viewer.Publication() != heldID {
		t.Fatalf("viewer publication changed to %s without adoption", viewer.Publication())
	}

	for range 3 {
		if _, err := publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := viewer.AdoptLatest(); !errors.Is(err, player.ErrWindowExpired) {
		t.Fatalf("adopt after expiry error=%v, want ErrWindowExpired", err)
	}
	if viewer.Publication() != heldID {
		t.Fatalf("failed adoption changed viewer from %s to %s", heldID, viewer.Publication())
	}

	historical, err := publisher.CompareReleases(original.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(historical.Ranges) != 1 || historical.Ranges[0].Start != media.SegmentDuration || historical.Ranges[0].End != 2*media.SegmentDuration {
		t.Fatalf("historical comparison ranges=%+v", historical.Ranges)
	}
	if viewer.Publication() != heldID {
		t.Fatalf("historical query changed viewer publication to %s", viewer.Publication())
	}

	s := store.Open(root)
	if !s.VersionExists(heldID) {
		t.Fatal("expired media held by a viewer was reclaimed before Close")
	}
	if s.VersionExists(first.ID) || s.VersionExists(withdrawn.ID) {
		t.Fatal("unreferenced expired media was retained to preserve release history")
	}
	if err := viewer.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := publisher.PublishNext()
	if err != nil {
		t.Fatal(err)
	}
	if s.VersionExists(heldID) {
		t.Fatal("held media was not reclaimed after release")
	}

	reopened, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := reopened.ReleaseNotes()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 11 {
		t.Fatalf("decision history length=%d, want 11", len(notes))
	}
	for _, id := range []string{original.ID, first.ID, second.ID, withdrawn.ID} {
		note, err := reopened.ReleaseNote(id)
		if err != nil {
			t.Fatalf("decision %s missing after media reclaim: %v", id, err)
		}
		if len(note.Changes) != len(media.Variants) {
			t.Fatalf("decision %s lost per-variant checksums: %+v", id, note)
		}
	}
	secondNote, err := reopened.ReleaseNote(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondNote.ParentID != first.ID || secondNote.WithdrawnID != "" {
		t.Fatalf("second decision lost parent relation: %+v", secondNote)
	}
	withdrawnNote, err := reopened.ReleaseNote(withdrawn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawnNote.ParentID != second.ID || withdrawnNote.WithdrawnID != second.ID {
		t.Fatalf("withdrawal decision lost parent/withdrawn relation: %+v", withdrawnNote)
	}
	afterReopen, err := reopened.CompareReleases(first.ID, withdrawn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterReopen.Ranges) != 0 {
		t.Fatalf("withdrawn content differs from its adopted parent: %+v", afterReopen.Ranges)
	}

	currentViewer, currentIndex, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	defer currentViewer.Close()
	if currentIndex.ID != current.ID {
		t.Fatalf("current publication=%s, want %s", currentIndex.ID, current.ID)
	}
	if _, err := reopened.ReleaseNote(second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.CompareReleases(original.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	origin, content, err := currentViewer.AdoptedReleaseID("base", 5)
	if err != nil {
		t.Fatal(err)
	}
	wantOrigin := publisher.VersionID(8)
	if origin != wantOrigin || content.AdoptedFrom != wantOrigin {
		t.Fatalf("current adoption origin=%s adopted_from=%s, want %s", origin, content.AdoptedFrom, wantOrigin)
	}
	if currentViewer.Publication() != current.ID {
		t.Fatalf("historical query changed current player to %s", currentViewer.Publication())
	}
	if _, err := os.Stat(filepath.Join(root, "publish", "versions", second.ID, "release.json")); !os.IsNotExist(err) {
		t.Fatalf("reclaimed version still contains release.json, err=%v", err)
	}
}

func TestPublisherMigratesLegacyVersionReleaseNotes(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 4)
	if err != nil {
		t.Fatal(err)
	}
	published, err := publisher.PublishNext()
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(root, "publish", "versions", published.ID, store.ReleaseNoteName)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "publish", "decisions", published.ID+".json"), legacyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "publish", "decisions")); err != nil {
		t.Fatal(err)
	}

	reopened, err := live.NewPublisher(root, "news", 4)
	if err != nil {
		t.Fatal(err)
	}
	note, err := reopened.ReleaseNote(published.ID)
	if err != nil {
		t.Fatal(err)
	}
	if note.ID != published.ID || note.Kind != store.ReleasePublish || len(note.Changes) != len(media.Variants) {
		t.Fatalf("legacy decision migration lost metadata: %+v", note)
	}
}

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
