package events

import "testing"

func TestHubPublishesMonotonicRevisionsAndCoalescesSlowConsumers(t *testing.T) {
	hub := New("stream-test")
	updates, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	first := hub.Publish("instance.changed", "one")
	second := hub.Publish("instance.changed", "two")
	if first.Revision != 1 || second.Revision != 2 {
		t.Fatalf("revisions = %d, %d; want 1, 2", first.Revision, second.Revision)
	}
	latest := <-updates
	if latest.Revision != 2 || latest.ResourceID != "two" {
		t.Fatalf("latest event = %+v; want revision 2 for resource two", latest)
	}
	streamID, revision := hub.Snapshot()
	if streamID != "stream-test" || revision != 2 {
		t.Fatalf("snapshot = %q/%d; want stream-test/2", streamID, revision)
	}
}
