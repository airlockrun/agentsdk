package agentsdk

// accessRank totally orders the three access levels so accessSatisfies can
// answer "is the caller's level high enough for this requirement?" with a
// single integer comparison. Higher rank = broader privilege.
func accessRank(a Access) int {
	switch a {
	case AccessAdmin:
		return 3
	case AccessUser:
		return 2
	case AccessPublic:
		return 1
	}
	return -1
}

// accessSatisfies reports whether a caller at level `caller` may invoke
// something registered at level `required`. Empty `required` defaults to
// AccessUser (matches existing implicit behavior).
func accessSatisfies(caller, required Access) bool {
	if required == "" {
		required = AccessUser
	}
	return accessRank(caller) >= accessRank(required)
}
