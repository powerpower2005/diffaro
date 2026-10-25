package profile

import "testing"

func TestRepositoryTransportIdentity(t *testing.T) {
	const repository = "https://github.example.com/team/charts"
	for _, value := range []string{"git@github.example.com:team/charts.git", "ssh://git@github.example.com/team/charts.git", "ssh://git@GITHUB.EXAMPLE.COM:22/team/charts", "https://github.example.com:443/team/charts.git/"} {
		if !SameRepository(repository, value) || !SameRepository(value, repository) {
			t.Errorf("same repository not recognized: %s", value)
		}
	}
	for _, value := range []string{"git@attacker.example.com:team/charts.git", "git@github.example.com:other/charts.git", "ssh://git@github.example.com:2222/team/charts", "ssh://admin@github.example.com/team/charts", "ssh://git:password@github.example.com/team/charts", "http://github.example.com/team/charts", "https://github.example.com/team/charts?x=y", "ssh://git@github.example.com/team/%63harts", "git@github.example.com:team/../charts", "git@github.example.com:/team/charts", "ssh://git@github.example.com//team/charts"} {
		if SameRepository(repository, value) {
			t.Errorf("unsafe alias accepted: %s", value)
		}
	}
}
