package mergequeue

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStackMarkerRoundTrips(t *testing.T) {
	stack := Stack{
		Base:    sha('b'),
		Members: []Member{{Number: 12, SHA: sha('c')}, {Number: 15, SHA: sha('d')}},
		Retry:   1,
		At:      time.Date(2026, 10, 11, 12, 30, 0, 0, time.UTC),
	}
	body := stackComment(stack, 1, []Own{{Head: sha('e'), Upstream: sha('f')}})
	if !strings.HasPrefix(body, StatusPrefix+"\n") {
		t.Fatalf("status comment does not start with the status prefix:\n%s", body)
	}
	parsed, ok := ParseStack(body)
	if !ok || !reflect.DeepEqual(parsed, stack) {
		t.Fatalf("ParseStack = %+v, %v; want %+v", parsed, ok, stack)
	}
	owns := ParseOwns(body)
	want := []Own{{Head: sha('d'), Upstream: sha('c')}, {Head: sha('e'), Upstream: sha('f')}}
	if !reflect.DeepEqual(owns, want) {
		t.Fatalf("ParseOwns = %+v, want %+v", owns, want)
	}
	if _, ok := ParseStack(stackComment(stack, 0, nil)); ok {
		t.Fatal("a non-top member's comment must not carry the stack marker")
	}
}

func TestParseStackRejectsMalformedMarkers(t *testing.T) {
	for _, body := range []string{
		"<!-- runneryard-merge-queue:stack base=" + sha('b') + " prs=1@" + sha('c') + ",1@" + sha('d') + " retry=0 at=1 -->",
		"<!-- runneryard-merge-queue:stack base=" + sha('b') + " prs=1@abc retry=0 at=1 -->",
		"<!-- runneryard-merge-queue:stack base=" + sha('b') + " prs=1@" + sha('c') + " retry=2 at=1 -->",
		"<!-- runneryard-merge-queue:stack base=" + sha('b') + " prs=0@" + sha('c') + " retry=0 at=1 -->",
		"<!-- multitap-merge-queue:stack base=" + sha('b') + " prs=1@" + sha('c') + " retry=0 at=1 -->",
	} {
		if stack, ok := ParseStack(body); ok {
			t.Errorf("ParseStack(%q) = %+v, want no stack", body, stack)
		}
	}
}

func TestCommentsKeepOwnRecordsAndLandedMarker(t *testing.T) {
	owns := []Own{{Head: sha('1'), Upstream: sha('2')}, {Head: sha('3'), Upstream: sha('4')}, {Head: sha('5'), Upstream: sha('6')}}
	for _, body := range []string{
		waitingComment(1, 2, owns),
		ineligibleComment("it is a draft", "queue", owns),
		rejectedComment("boom", "queue", []string{"https://example.invalid"}, owns),
		requeuedComment("moved", owns),
	} {
		if got := ParseOwns(body); !reflect.DeepEqual(got, owns[:maxOwns]) {
			t.Fatalf("ParseOwns(%q) = %+v, want the first %d records kept", body, got, maxOwns)
		}
	}
	stack := Stack{Base: sha('b'), Members: []Member{{Number: 4, SHA: sha('c')}}}
	landed, ok := ParseLanded(landedComment(stack))
	if !ok || landed.Head != sha('c') {
		t.Fatalf("ParseLanded = %+v, %v", landed, ok)
	}
}
