package main

import "testing"

// sampleUsage mirrors a real GetUsageLimits response from
// management.us-east-1.kiro.dev.
const sampleUsage = `{
  "daysUntilReset": null,
  "limits": null,
  "nextDateReset": 1785542400.0,
  "overageConfiguration": { "overageStatus": "DISABLED" },
  "subscriptionInfo": {
    "subscriptionTitle": "KIRO PRO+",
    "type": "Q_DEVELOPER_STANDALONE_PRO_PLUS"
  },
  "usageBreakdownList": [
    {
      "currency": "USD",
      "currentUsage": 1884,
      "currentUsageWithPrecision": 1884.27,
      "displayName": "Credit",
      "displayNamePlural": "Credits",
      "freeTrialInfo": null,
      "overageCap": 10000,
      "overageCapWithPrecision": 10000.0,
      "overageRate": 0.04,
      "resourceType": "CREDIT",
      "unit": "INVOCATIONS",
      "usageLimit": 2000,
      "usageLimitWithPrecision": 2000.0
    }
  ],
  "userInfo": { "email": "user@example.com", "userId": "d-abc.123" }
}`

func TestParseKiroUsage(t *testing.T) {
	u, err := parseKiroUsage([]byte(sampleUsage))
	if err != nil {
		t.Fatalf("parseKiroUsage: %v", err)
	}
	if u.SubscriptionTitle != "KIRO PRO+" {
		t.Errorf("subscription title = %q", u.SubscriptionTitle)
	}
	if u.SubscriptionType != "Pro+" {
		t.Errorf("subscription type = %q (want friendly Pro+)", u.SubscriptionType)
	}
	if u.Email != "user@example.com" {
		t.Errorf("email = %q", u.Email)
	}
	if u.ResetAt == "" {
		t.Error("reset_at not populated from nextDateReset")
	}
	if u.OverageStatus != "DISABLED" {
		t.Errorf("overage status = %q", u.OverageStatus)
	}
	c := u.Credit
	if c == nil {
		t.Fatal("credit breakdown missing")
	}
	if c.Limit != 2000 {
		t.Errorf("limit = %v, want 2000", c.Limit)
	}
	if c.Used != 1884.27 {
		t.Errorf("used = %v, want 1884.27 (precise value)", c.Used)
	}
	if got := c.Remaining; got < 115.72 || got > 115.74 {
		t.Errorf("remaining = %v, want ~115.73", got)
	}
	if c.Unit != "次调用" || c.Currency != "USD" {
		t.Errorf("unit/currency = %q/%q", c.Unit, c.Currency)
	}
	if len(u.Raw) == 0 {
		t.Error("raw payload not retained")
	}
}

func TestParseKiroUsageFreeTrialMerged(t *testing.T) {
	body := `{
      "subscriptionInfo": {"subscriptionTitle": "Free"},
      "usageBreakdownList": [{
        "resourceType": "CREDIT",
        "currentUsageWithPrecision": 10,
        "usageLimitWithPrecision": 50,
        "freeTrialInfo": {
          "freeTrialStatus": "ACTIVE",
          "currentUsageWithPrecision": 5,
          "usageLimitWithPrecision": 100
        }
      }]
    }`
	u, err := parseKiroUsage([]byte(body))
	if err != nil {
		t.Fatalf("parseKiroUsage: %v", err)
	}
	c := u.Credit
	if c == nil {
		t.Fatal("credit missing")
	}
	if !c.FreeTrialActive {
		t.Error("free trial should be active")
	}
	// Base 50 + trial 100 = 150 limit; base 10 + trial 5 = 15 used.
	if c.Limit != 150 || c.Used != 15 || c.Remaining != 135 {
		t.Errorf("merged totals: used=%v limit=%v remaining=%v; want 15/150/135", c.Used, c.Limit, c.Remaining)
	}
}

func TestFriendlySubscriptionType(t *testing.T) {
	cases := map[string]string{
		"Q_DEVELOPER_STANDALONE_PRO_PLUS": "Pro+",
		"Q_DEVELOPER_STANDALONE_PRO":      "Pro",
		"Q_DEVELOPER_STANDALONE_FREE":     "Free",
		"Q_DEVELOPER_POWER":               "Power",
		"ENTERPRISE":                      "Enterprise",
		"":                                "",
		"SOME_NEW_TIER":                   "Some New Tier", // fallback
	}
	for in, want := range cases {
		if got := friendlySubscriptionType(in); got != want {
			t.Errorf("friendlySubscriptionType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFriendlyUnit(t *testing.T) {
	cases := map[string]string{
		"INVOCATIONS": "次调用",
		"CREDIT":      "credits",
		"TOKENS":      "tokens",
		"":            "",
		"WEIRD_UNIT":  "Weird Unit", // fallback
	}
	for in, want := range cases {
		if got := friendlyUnit(in); got != want {
			t.Errorf("friendlyUnit(%q) = %q, want %q", in, got, want)
		}
	}
}
