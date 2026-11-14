// Package clustertest holds the tests that run against the real three-node
// cluster of contrib/compose/cluster.yaml: three rqlite nodes and three
// robinmail nodes in Docker, and next to them a single robinmail with a
// database of its own that the cluster exchanges mail with. They kill,
// freeze and restart containers, so they only run when asked:
//
//	make cluster-test
//
// or, by hand:
//
//	contrib/compose/cluster.sh up
//	ROBINMAIL_TEST_CLUSTER=1 go test -count=1 -v -timeout 30m ./internal/clustertest
//	contrib/compose/cluster.sh down
//
// Without ROBINMAIL_TEST_CLUSTER they are skipped.
package clustertest
