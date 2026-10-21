package proxmox

import (
	"context"

	"github.com/hazame-hub/accretion/plan"
)

// ObservedSDN reads the cluster's SDN as the planner compares it. It fails,
// rather than answering empty, when the token may not read the SDN.
func (s *session) ObservedSDN(ctx context.Context) (plan.ObservedSDN, error) {
	st, err := s.c.SDN(ctx)
	if err != nil {
		return plan.ObservedSDN{}, err
	}
	return st.Observed(), nil
}

// ObservedCapacity reads the cluster's memory as the planner sizes a plan
// against it.
func (s *session) ObservedCapacity(ctx context.Context) (plan.Capacity, error) {
	c, err := s.c.Capacity(ctx)
	if err != nil {
		return plan.Capacity{}, err
	}
	return c.Observed(), nil
}
