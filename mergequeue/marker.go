package mergequeue

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The queue keeps one status comment per pull request and edits it in
// place. Hidden markers in that comment carry the little state a tick needs
// from the previous one. A tick trusts a marker only in a comment written by
// the queue's own bot account (see GitHub.StatusComment), so nobody can
// forge one by commenting.
const (
	markerPrefix = "<!-- runneryard-merge-queue:"
	// StatusPrefix opens every status comment the queue writes.
	StatusPrefix = markerPrefix + "status -->"
)

// Member is one pull request in a stack, with the commit the queue pushed
// to its branch.
type Member struct {
	Number int
	SHA    string
}

// Stack is a group of queued pull requests rebased on top of each other
// above Base, the target branch commit it was built on. The marker lives
// on the top pull request, whose CI tests the whole stack.
type Stack struct {
	Base    string
	Members []Member
	// Retry is 1 once the queue re-ran the stack's failed jobs.
	Retry int
	// At is when the stack was formed, or retried: check timeouts count
	// from it.
	At time.Time
}

// Top is the last member, whose commit contains the whole stack.
func (s Stack) Top() Member { return s.Members[len(s.Members)-1] }

// Numbers lists the member pull request numbers in stack order.
func (s Stack) Numbers() []int {
	numbers := make([]int, len(s.Members))
	for i, member := range s.Members {
		numbers[i] = member.Number
	}
	return numbers
}

// Own records which commits on a pull request branch are the pull request's
// own: upstream..head. Once the queue rebased a branch onto batchmates, the
// branch also holds their commits; without this record, the next stack
// would carry them along even after they left the queue. A comment keeps
// up to two: the one for the branch the queue is about to push, and the
// one still valid if that push never happens.
type Own struct {
	Head, Upstream string
}

// Landed records that the queue fast-forwarded the target to Head, so the
// pull request is waiting for GitHub to mark it merged.
type Landed struct {
	Head string
}

var (
	stackPattern  = regexp.MustCompile(`<!-- runneryard-merge-queue:stack base=([0-9a-f]{40}) prs=([0-9]+@[0-9a-f]{40}(?:,[0-9]+@[0-9a-f]{40})*) retry=([01]) at=([0-9]+) -->`)
	ownPattern    = regexp.MustCompile(`<!-- runneryard-merge-queue:own head=([0-9a-f]{40}) upstream=([0-9a-f]{40}) -->`)
	landedPattern = regexp.MustCompile(`<!-- runneryard-merge-queue:landed head=([0-9a-f]{40}) -->`)
)

func stackMarker(stack Stack) string {
	members := make([]string, len(stack.Members))
	for i, member := range stack.Members {
		members[i] = strconv.Itoa(member.Number) + "@" + member.SHA
	}
	return fmt.Sprintf("%sstack base=%s prs=%s retry=%d at=%d -->", markerPrefix, stack.Base, strings.Join(members, ","), stack.Retry, stack.At.Unix())
}

func ownMarker(own Own) string {
	return fmt.Sprintf("%sown head=%s upstream=%s -->", markerPrefix, own.Head, own.Upstream)
}

func landedMarker(landed Landed) string {
	return fmt.Sprintf("%slanded head=%s -->", markerPrefix, landed.Head)
}

// ParseStack returns the stack marker in body, if any.
func ParseStack(body string) (Stack, bool) {
	match := stackPattern.FindStringSubmatch(body)
	if match == nil {
		return Stack{}, false
	}
	stack := Stack{Base: match[1], Retry: int(match[3][0] - '0')}
	seen := map[int]bool{}
	for _, entry := range strings.Split(match[2], ",") {
		number, sha, _ := strings.Cut(entry, "@")
		n, err := strconv.Atoi(number)
		if err != nil || n <= 0 || seen[n] {
			return Stack{}, false
		}
		seen[n] = true
		stack.Members = append(stack.Members, Member{Number: n, SHA: sha})
	}
	at, err := strconv.ParseInt(match[4], 10, 64)
	if err != nil {
		return Stack{}, false
	}
	stack.At = time.Unix(at, 0).UTC()
	return stack, true
}

// maxOwns bounds the own-commits markers one comment keeps.
const maxOwns = 2

// ParseOwns returns the own-commits markers in body, newest first.
func ParseOwns(body string) []Own {
	var owns []Own
	for _, match := range ownPattern.FindAllStringSubmatch(body, maxOwns) {
		owns = append(owns, Own{Head: match[1], Upstream: match[2]})
	}
	return owns
}

// ParseLanded returns the landed marker in body, if any.
func ParseLanded(body string) (Landed, bool) {
	match := landedPattern.FindStringSubmatch(body)
	if match == nil {
		return Landed{}, false
	}
	return Landed{Head: match[1]}, true
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func refs(numbers []int) string {
	parts := make([]string, len(numbers))
	for i, number := range numbers {
		parts[i] = "#" + strconv.Itoa(number)
	}
	return strings.Join(parts, ", ")
}

func statusBody(text string, markers ...string) string {
	lines := append([]string{StatusPrefix, "Merge queue: " + text}, markers...)
	return strings.Join(lines, "\n")
}

// ownMarkers renders the own-commits markers a pull request already has, so
// rewriting its status comment never loses them.
func ownMarkers(owns []Own) []string {
	var markers []string
	for _, own := range owns {
		if len(markers) == maxOwns {
			break
		}
		markers = append(markers, ownMarker(own))
	}
	return markers
}

// stackOwn is the own-commits record of the member at index.
func stackOwn(stack Stack, index int) Own {
	upstream := stack.Base
	if index > 0 {
		upstream = stack.Members[index-1].SHA
	}
	return Own{Head: stack.Members[index].SHA, Upstream: upstream}
}

func waitingComment(position, total int, owns []Own) string {
	return statusBody(fmt.Sprintf("position %d of %d, waiting for the stack ahead to resolve.", position, total), ownMarkers(owns)...)
}

func ineligibleComment(reason, label string, owns []Own) string {
	return statusBody(fmt.Sprintf("not queued: %s. Fix it and the queue picks this pull request up again while the `%s` label stays on.", reason, label), ownMarkers(owns)...)
}

// stackComment is the status of a member of a stack in flight. Every member
// records its own commits in the stack, then keep (the record valid for its
// branch before the stack); the top member also carries the stack marker.
func stackComment(stack Stack, index int, keep []Own) string {
	member := stack.Members[index]
	markers := ownMarkers(append([]Own{stackOwn(stack, index)}, keep...))
	var others []int
	for _, number := range stack.Numbers() {
		if number != member.Number {
			others = append(others, number)
		}
	}
	with := ""
	if len(others) > 0 {
		with = " with " + refs(others)
	}
	if index == len(stack.Members)-1 {
		text := fmt.Sprintf("testing this branch as the top of a stack%s, rebased onto %s as %s.", with, shortSHA(stack.Base), shortSHA(member.SHA))
		if stack.Retry > 0 {
			text = fmt.Sprintf("re-running the failed jobs once on this stack%s before giving up (%s).", with, shortSHA(member.SHA))
		}
		return statusBody(text, append(markers, stackMarker(stack))...)
	}
	return statusBody(fmt.Sprintf("rebased into a stack%s as %s. CI runs once, on the top pull request.", with, shortSHA(member.SHA)), markers...)
}

// landedComment is the status of the member at index once its stack landed.
// The marker holds that member's own head, which GitHub keeps reporting
// until it marks the pull request merged.
func landedComment(stack Stack, index int) string {
	var batchmates []int
	for i, member := range stack.Members {
		if i != index {
			batchmates = append(batchmates, member.Number)
		}
	}
	with := ""
	if len(batchmates) > 0 {
		with = " together with " + refs(batchmates)
	}
	return statusBody(fmt.Sprintf("landed on the target as %s%s.", shortSHA(stack.Top().SHA), with), landedMarker(Landed{Head: stack.Members[index].SHA}))
}

func rejectedComment(reason, label string, urls []string, owns []Own) string {
	text := fmt.Sprintf("rejected (%s). Re-apply the `%s` label once this is fixed.", reason, label)
	if len(urls) > 0 {
		text += "\n\n- " + strings.Join(urls, "\n- ")
	}
	return statusBody(text, ownMarkers(owns)...)
}

func requeuedComment(reason string, owns []Own) string {
	return statusBody(fmt.Sprintf("back in the queue (%s).", reason), ownMarkers(owns)...)
}
