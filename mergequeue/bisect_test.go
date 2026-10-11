package mergequeue

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	for _, test := range []struct {
		prs         []int
		front, back []int
	}{
		{[]int{1}, []int{1}, nil},
		{[]int{1, 2}, []int{1}, []int{2}},
		{[]int{1, 2, 3}, []int{1, 2}, []int{3}},
		{[]int{1, 2, 3, 4}, []int{1, 2}, []int{3, 4}},
		{[]int{1, 2, 3, 4, 5}, []int{1, 2, 3}, []int{4, 5}},
	} {
		front, back := Split(test.prs)
		if !reflect.DeepEqual(front, test.front) || !reflect.DeepEqual(back, test.back) {
			t.Fatalf("Split(%v) = %v, %v; want %v, %v", test.prs, front, back, test.front, test.back)
		}
	}
}

func TestSplitBisectsToSingletonsWithinLog2Rounds(t *testing.T) {
	batch := []int{1, 2, 3}
	rounds := 0
	var queue [][]int
	queue = append(queue, batch)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if len(current) == 1 {
			continue
		}
		rounds++
		front, back := Split(current)
		queue = append(queue, front, back)
	}
	if rounds > 2 {
		t.Fatalf("bisecting a batch of %d took %d rounds, want at most ceil(log2(3)) = 2", len(batch), rounds)
	}
}
