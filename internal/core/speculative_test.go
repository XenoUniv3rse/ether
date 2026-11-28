package core

import (
	"context"
	"testing"
)

func TestExplicitPlayIsSetOnlyByAMark(t *testing.T) {
	bare := WithIndexerPurpose(context.Background(), IndexerPurposePlayback)
	if ExplicitPlay(bare) {
		t.Fatal("a playback purpose alone reads as explicit Play")
	}
	played := WithExplicitPlay(bare)
	if !ExplicitPlay(played) || !ExplicitPlay(context.WithoutCancel(played)) {
		t.Fatal("explicit Play lost, or lost on a detached context")
	}

	shared, latch := WithExplicitPlayLatch(context.WithoutCancel(bare))
	inner, _ := WithExplicitPlayLatch(shared)
	if ExplicitPlay(shared) || ExplicitPlay(inner) {
		t.Fatal("shared work started by no Play reads as explicit Play")
	}
	latch()
	if !ExplicitPlay(shared) || !ExplicitPlay(inner) {
		t.Fatal("a Play joining shared work did not reach it, or work nested in it")
	}

	led, _ := WithExplicitPlayLatch(context.WithoutCancel(played))
	if !ExplicitPlay(led) {
		t.Fatal("shared work started by a Play does not read as explicit Play")
	}
}
