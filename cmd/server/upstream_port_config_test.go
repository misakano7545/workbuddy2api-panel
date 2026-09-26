package main

import "testing"

func TestBudgetAndJitterRejectBadValues(t *testing.T) {
	c := Default()
	c.Budget.DailyCreditLimit = -1
	if err := c.normalize(); err == nil {
		t.Fatal("negative budget should fail")
	}
	c = Default()
	c.Schedule.JitterMinutes = 1441
	if err := c.normalize(); err == nil {
		t.Fatal("jitter over a day should fail")
	}
	c = Default()
	c.Alerting.Enabled = true
	if err := c.normalize(); err == nil {
		t.Fatal("alerting without webhook should fail")
	}
}
