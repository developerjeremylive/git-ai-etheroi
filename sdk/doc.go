// Package sdk is the public Go client for Graft, the server-centric
// distribution of Gitaly.
//
// The SDK is built for always-online clients: agents, CI jobs and sparse
// working copies that read exactly the files and objects they need and land
// changes through the server instead of pushing branches and merging on the
// client. Every operation maps onto one RPC:
//
//   - ReadFile, ListTree, ResolveRevision read the tree (CommitService and
//     ObjectService).
//   - GetObjects fetches raw objects by ID, resuming interrupted transfers.
//   - CommitFiles builds a commit on the server from a list of actions.
//   - Land lands a commit on a branch, rebasing or merging onto the current
//     tip and retrying concurrent updates on the server. It retries the
//     calls the server answers with Unavailable (see UnavailableRetry).
//   - Blame and PathHistory answer history questions from derived data.
//   - Changesets and WatchChangesets read the repository's changeset log.
//   - Create creates an empty repository, and HasBranches tells whether it
//     is still empty (RepositoryService; operators only, with the shared
//     token).
//
// Errors are standard gRPC errors. Structured details of landing and commit
// failures are available through LandError, CommitFilesError and the Is*
// helpers.
//
// Non-test code in this package must not import internal packages.
package sdk
