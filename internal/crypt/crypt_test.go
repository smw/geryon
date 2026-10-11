package crypt

import "testing"

// Reference values produced by the system crypt(3) (via perl's crypt).
func TestCrypt(t *testing.T) {
	cases := []struct{ pw, setting, want string }{
		{"Hello world!", "$6$saltstring", "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{"pw", "$5$abc", "$5$abc$/LKlXd6vuvdFX3oWbf2vGk3DgflzzsvOtMZsiOxqqY5"},
		{
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"$6$rounds=1500$0123456789abcdefXX",
			"$6$rounds=1500$0123456789abcdef$if25.kICma/X/WmD3ipB/dNfntq9gRxzCFz01hRZvVnszBY9FfzXlqkr6LMUQxSgkWHrBk/c5IcXIIcYYFPRP0",
		},
		{"password", "pa", "papAq5PwY/QQM"},
		{"kittycat99", "ki", "kiAg8lV8RVkx2"},
	}
	for _, c := range cases {
		got, err := Crypt(c.pw, c.setting)
		if err != nil {
			t.Errorf("Crypt(%q, %q): %v", c.pw, c.setting, err)
			continue
		}
		if got != c.want {
			t.Errorf("Crypt(%q, %q) = %q, want %q", c.pw, c.setting, got, c.want)
		}
		if !Verify(c.pw, c.want) {
			t.Errorf("Verify(%q, %q) = false", c.pw, c.want)
		}
		if Verify(c.pw+"x", c.want) && len(c.want) > 13 {
			t.Errorf("Verify accepted a wrong password for %q", c.want)
		}
	}
}

func TestVerifyRejectsUnusable(t *testing.T) {
	for _, stored := range []string{"*0", "", "$1$abc$xyz"} {
		if Verify("anything", stored) {
			t.Errorf("Verify accepted unusable hash %q", stored)
		}
	}
}
