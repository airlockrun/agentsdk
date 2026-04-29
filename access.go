package agentsdk

// accessRank totally orders the four access levels so accessSatisfies can
// answer "is the caller's level high enough for this requirement?" with a
// single integer comparison. AccessInternal sits below AccessPublic at -1
// because it represents "not reachable by any caller" — only builder Go
// code holds the in-process handle. Higher rank = broader privilege.
func accessRank(a Access) int {
	switch a {
	case AccessAdmin:
		return 3
	case AccessUser:
		return 2
	case AccessPublic:
		return 1
	case AccessInternal:
		return -1 // never matches anything from the JS / external side
	}
	return -1
}

// accessSatisfies reports whether a caller at level `caller` may invoke
// something registered at level `required`. Empty `required` defaults to
// AccessUser (matches existing implicit behavior). Internal-only items
// are never satisfied — there is no caller level that can reach them
// from JS or external callbacks; only builder Go code touches them.
func accessSatisfies(caller, required Access) bool {
	if required == "" {
		required = AccessUser
	}
	if required == AccessInternal {
		return false
	}
	return accessRank(caller) >= accessRank(required)
}
