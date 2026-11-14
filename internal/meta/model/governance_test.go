package model

import "testing"

func TestClassificationRankSupportsBothEnums(t *testing.T) {
	cases := map[string]int{
		"secret": 3, "high": 3, "SECRET": 3,
		"confidential": 2, "medium": 2,
		"internal": 1, "public": 1, "low": 1,
		"": 0, "unknown": 0,
	}
	for in, want := range cases {
		if got := ClassificationRank(in); got != want {
			t.Fatalf("ClassificationRank(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestClassificationName(t *testing.T) {
	cases := map[int]string{3: "high", 2: "medium", 1: "low", 0: "none", 9: "none"}
	for in, want := range cases {
		if got := ClassificationName(in); got != want {
			t.Fatalf("ClassificationName(%d) = %q, want %q", in, got, want)
		}
	}
}
