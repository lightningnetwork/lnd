package macaroons

import (
	"fmt"
	"reflect"
	"testing"
)

func TestRouteConstraintsParser(t *testing.T) {
	constrStrings := []string{
		"path[0]      in {n1, n2}",
		"path[-1] not in {n1}",
		"path[10] not in {}",
		"path[0]      in {n1,  n2,n3}",
		"path[-1] notin  {n1,n2 }",
	}
	constraints := []routeConstraint{
		{0, false, []string{"n1", "n2"}},
		{-1, true, []string{"n1"}},
		{10, true, []string{}},
		{0, false, []string{"n1", "n2", "n3"}},
		{-1, true, []string{"n1", "n2"}},
	}
	for i := 0; i < len(constrStrings); i++ {
		constraint, err := parseRouteConstraint(constrStrings[i])
		if err != nil {
			msg := fmt.Sprintf(" in \"%s\"", constrStrings[i])
			t.Errorf(err.Error() + msg)
		}
		if reflect.DeepEqual(constraint, constraints[i]) {
			// constraint.index != constraints[i].index ||
			//	constraint.negate != constraints[i].negate ||
			//	constraint.nodeSet != constraints[i].nodeSet {
			msg := "Parsing \"%s\" returned incorrect route constraint"
			t.Errorf(msg, constrStrings[i])
		}
	}
}
