package mergequeue

// Split divides prs (already ordered, len(prs) > 1) into two halves for
// bisection on a batch failure: a front half and a back half, each stacked
// again on the base the failed batch was tested against, never on each
// other. PlanTick does not recurse in one tick: each half becomes its own
// stack, and the same success and failure handling applies to it on a later
// tick, splitting it again if it is itself a multi-PR stack that fails. The
// search therefore takes at most ceil(log2(len(prs))) rounds.
func Split(prs []int) (front, back []int) {
	if len(prs) < 2 {
		return prs, nil
	}
	mid := (len(prs) + 1) / 2
	front = append([]int{}, prs[:mid]...)
	back = append([]int{}, prs[mid:]...)
	return front, back
}
