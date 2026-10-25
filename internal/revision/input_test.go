package revision

import "testing"

func TestPullRequestInput(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  int
		bad   bool
	}{
		{"", 0, false},
		{"123", 123, false},
		{"https://github.example.com/team/charts/pull/123", 123, false},
		{"https://attacker.example/team/charts/pull/123", 0, true},
		{"https://github.example.com/team/other/pull/123", 0, true},
		{"https://secret@github.example.com/team/charts/pull/123", 0, true},
		{"https://github.example.com/team/charts/pull/123?token=secret", 0, true},
		{"https://github.example.com/team/charts/pull/%31", 0, true},
		{"https://github.example.com/team/charts/pull/123/files", 0, true},
		{"http://github.example.com/team/charts/pull/123", 0, true},
		{"0", 0, true}, {"-1", 0, true}, {"+1", 0, true},
		{"9999999999999999999999999999999", 0, true},
	} {
		t.Run(tt.input, func(t *testing.T) {
			got, err := PullRequestNumber(tt.input, "https://github.example.com/team/charts.git")
			if got != tt.want || (err != nil) != tt.bad {
				t.Fatalf("got %d, %v; want %d, bad=%v", got, err, tt.want, tt.bad)
			}
		})
	}
}
