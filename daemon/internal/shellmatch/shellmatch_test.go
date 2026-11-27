package shellmatch

import "testing"

func TestMatchPublishCommand(t *testing.T) {
	cases := []struct{ cmd, want string }{
		// Compound commands and redirections lead nothing: the publish inside them is still one.
		{"if true; then git push origin main; fi", "git push"},
		{"if gh pr checks 177; then gh pr merge 177 --squash; fi", "gh pr merge"},
		{`for b in main; do git push origin "$b"; done`, "git push"},
		{"while true; do git push; done", "git push"},
		{"true && ! git push", "git push"},
		{"if x; then :; elif y; then git push; else :; fi", "git push"},
		{"</dev/null git push", "git push"},
		{"2>&1 git push", "git push"},
		{">&2 git push", "git push"},
		{"&>/dev/null git push", "git push"},
		{"> out.txt git push", "git push"},
		{"2>/dev/null </dev/null gh pr merge 5", "gh pr merge"},
		{"git push 2>&1 | tee log", "git push"},
		{"make 2>&1 && git push", "git push"},
		{"git push", "git push"},
		{"git -C . -c core.x=y push origin HEAD", "git push"},
		{"git --git-dir=.git push", "git push"},
		{"gh pr comment 5 -b x", "gh pr comment"},
		{"gh pr --repo o/r merge 5", "gh pr merge"},
		{"gh -R o/r pr comment 5 -b x", "gh pr comment"},
		{"gh --repo=o/r pr review 5 --approve", "gh pr review"},
		{"gh pr -R o/r edit 5 --title t", "gh pr edit"},
		{"gh issue -R o/r comment 3 --body hi", "gh issue comment"},
		{`bash -c "git push"`, "git push"},
		{`sh -lc 'cd x && git push origin HEAD'`, "git push"},
		{`bash -o pipefail -c "gh pr merge 5"`, "gh pr merge"},
		{`eval "git push"`, "git push"},
		{"env -i git push", "git push"},
		{"env -u HOME -C /tmp git push", "git push"},
		{"env FOO=1 BAR=2 git push", "git push"},
		{`env -S "git push"`, "git push"},
		{"FOO=1 sudo -u me nohup git push", "git push"},
		{"timeout -s KILL 30 git push", "git push"},
		{"echo x | xargs -n 1 git push", "git push"},
		{"(git push)", "git push"},
		{"{ git push; }", "git push"},
		{"ls $(git push)", "git push"},
		{"gh api repos/o/r/pulls/5/reviews -f body=x -f event=APPROVE", "gh api POST"},
		{"gh api -X POST repos/o/r/issues/5/comments", "gh api POST"},
		{"gh api --method=PATCH repos/o/r/pulls/5", "gh api PATCH"},
		{"gh api -XDELETE repos/o/r/issues/comments/9", "gh api DELETE"},
		{`gh api graphql -f query='mutation { addComment(input:{}) { clientMutationId } }'`, "gh api graphql mutation"},
		{"gh api repos/o/r/issues/5/comments -fbody=LGTM", "gh api POST"},
		{"gh api repos/o/r/pulls/5/reviews -Fevent=APPROVE", "gh api POST"},
		{`bash -c '(git push)'`, "git push"},
		{`echo "$(git push)"`, "git push"},
		// gh writes beyond the first list: every gh command but a read is a publish.
		{"gh pr create --title t --body b", "gh pr create"},
		{"gh pr ready 5", "gh pr ready"},
		{"gh pr reopen 5", "gh pr reopen"},
		{"gh pr -R o/r create -f", "gh pr create"},
		{"gh issue create -t t", "gh issue create"},
		{"gh issue close 3", "gh issue close"},
		{"gh issue edit 3 --add-label x", "gh issue edit"},
		{"gh release create v1 --notes x", "gh release create"},
		{"gh repo delete o/r --yes", "gh repo delete"},
		{"gh run rerun 9", "gh run rerun"},
		{"gh workflow run ci.yml", "gh workflow run"},
		{"gh secret set X -b y", "gh secret set"},
		{"gh label create bug", "gh label create"},
		{"gh extension install o/x", "gh extension"},
		{"gh alias set x 'pr merge'", "gh alias"},
		{"gh gist create f", "gh gist create"},
		{"gh some-new-group do", "gh some-new-group"},
		{"env GH_TOKEN=x gh pr merge 5", "gh pr merge"},
		// Reads stay allowed.
		{`grep -n "(git push)" docs.md`, ""},
		{`grep -rn "{gh pr merge}" .`, ""},
		{`echo "see (git push) docs"`, ""},
		{`grep -n '{git push}' docs.md`, ""},
		{"git status && git log --oneline", ""},
		// Reads stay reads inside the same wrappers.
		{"if git status; then git log; fi", ""},
		{"git status 2>&1 | head", ""},
		{"</dev/null git status", ""},
		{"for f in a b; do echo $f > out; done", ""},
		{"git -C . log push", ""},
		{"gh pr view 5 --comments", ""},
		{"gh pr list --search comment", ""},
		{"gh pr diff 5 -R o/r", ""},
		{"gh pr checks 5", ""},
		{"gh pr status", ""},
		{"gh pr", ""},
		{"gh issue view 3 --comments", ""},
		{"gh release download v1 -p x", ""},
		{"gh repo view o/r --json name", ""},
		{"gh run view 9 --log", ""},
		{"gh workflow list", ""},
		{"gh search issues bug -R o/r", ""},
		{"gh auth status", ""},
		{"gh status", ""},
		{"gh --version", ""},
		{"gh -R o/r pr list", ""},
		{"gh run --workflow ci.yml list", ""},
		{"gh run -w ci.yml list", ""},
		{"gh run -wci.yml -j 5 list", ""},
		{"gh run -R o/r --job 5 view", ""},
		{"gh api repos/o/r/pulls/5/comments", ""},
		{"gh api -X GET repos/o/r/pulls -f state=open", ""},
		{`gh api graphql -f query='query { viewer { login } }'`, ""},
		{`echo "do not git push"`, ""},
		{"bash ./scripts/check.sh", ""},
		{"go test ./...", ""},
	}
	for _, c := range cases {
		if got := MatchPublishCommand(c.cmd); got != c.want {
			t.Errorf("MatchPublishCommand(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestMatchPatternKill(t *testing.T) {
	for _, line := range []string{
		`pkill -f "sleep 30"`, `pkill sleep`, `killall node`, `killall5 -9`, `/usr/bin/pkill -9 -f check.sh`,
		`sudo pkill -f x`, `env FOO=1 pkill x`, `timeout 5 pkill x`, `bash -c "pkill -f x"`, `echo done; pkill x`,
		`ls && killall x`, `(pkill x)`, `if true; then pkill x; fi`, `nohup pkill x &`,
		`kill $(pgrep -f sleep)`, `kill -9 $(pgrep foo)`, "kill `pgrep foo`", `kill -9 "$(pidof node)"`,
		`pgrep -f sleep | xargs kill`, `pidof node | xargs -r kill -9`, `ps aux | grep sleep | awk '{print $2}' | xargs kill`,
		`kill $(ps aux | grep sleep | awk '{print $2}')`, `pgrep -f x | xargs -n1 kill -TERM`,
		`kill $(pgrep -G 1000)`, `pgrep -G 1000 | xargs kill`, `pid=$(pgrep -f sleep); kill $pid`, `for p in $(pgrep node); do kill $p; done`,
		`pgrep node | while read p; do kill $p; done`, `kill -9 $(/usr/bin/pgrep -f sleep)`, `kill $(command pgrep -f sleep)`,
		`kill $(ps -eo pid,cmd | awk '/sleep/ {print $1}')`, `echo x | xargs pkill`, `! pkill x`, `FOO=1 pkill x`,
	} {
		if got := MatchPatternKill(line); got == "" {
			t.Errorf("%q passed", line)
		}
	}
}

func TestMatchPatternKillLeavesAnAgentsOwnPidsAlone(t *testing.T) {
	for _, line := range []string{
		`kill 1234`, `kill -9 $!`, `kill "$pid"`, `kill $(cat pidfile)`, `kill -- -$pgid`, `kill $(pgrep -P $$)`,
		`pgrep -P 1234 | xargs kill`, `pgrep -s $sid | xargs kill`, `kill $(pgrep -g $pgid)`, `pgrep -f sleep`,
		`pgrep -f sleep; kill 123`, `kill $(pgrep -P1234)`, `kill $(pgrep --parent=1234)`, `kill -0 $(pgrep foo)`, `kill -l`,
		`pgrep -P $$ | xargs kill; pgrep -f server`, `git commit -m "x"; kill 123`, `pid=$(cat pidfile); kill $pid`, `for p in $(pgrep -P $$); do kill $p; done`, `kill $(cat pidfile) && pgrep -f sleep`, `kill $(cat pidfile); pidof node`, `cat pidfile | xargs kill`, `ps -o pid= --ppid $$`, `ps aux | grep sleep`,
		`grep pkill notes.md`, `echo "do not pkill"`, `git commit -m "no killall"`, `man pkill`, `which killall`,
		`go test ./... 2>&1 | tail`, `ls`, ``,
	} {
		if got := MatchPatternKill(line); got != "" {
			t.Errorf("%q refused as %q", line, got)
		}
	}
}
