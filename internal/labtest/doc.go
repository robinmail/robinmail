// Package labtest holds the tests that run against the interoperability lab
// of contrib/lab: one robinmail next to Postfix, Exim and OpenSMTPD in
// Docker, each with a domain of its own, a private DNS and a private
// certificate authority. The tests send mail both ways through the real
// servers and read it back: from the peers' Maildirs and from robinmail
// over JMAP. They stop and start containers, so they only run when asked:
//
//	make lab-test
//
// or, by hand:
//
//	contrib/lab/lab.sh up
//	ROBINMAIL_TEST_LAB=1 go test -count=1 -v -timeout 30m ./internal/labtest
//	contrib/lab/lab.sh down
//
// Without ROBINMAIL_TEST_LAB they are skipped. At the end the tests print
// the matrix: one row per case, one column per peer.
//
// What a test finds wrong in robinmail is fixed there, with a test in the
// package concerned that needs no Docker: the lab is where faults are
// found, not where they are guarded against.
package labtest
