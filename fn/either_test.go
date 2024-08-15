package fn

import (
	"testing"
	"testing/quick"
)

func TestPropConstructorEliminatorDuality(t *testing.T) {
	f := func(i int, s string, isRight bool) bool {
		Len := func(s string) int { return len(s) } // smh
		if isRight {
			v := ElimEither(
				NewRight[int, string](s),
				Iden[int],
				Len,
			)
			return v == Len(s)
		}

		v := ElimEither(
			NewLeft[int, string](i),
			Iden[int],
			Len,
		)
		return v == i
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPropWhenClauseExclusivity(t *testing.T) {
	f := func(i int, isRight bool) bool {
		var e Either[int, int]
		if isRight {
			e = NewRight[int, int](i)
		} else {
			e = NewLeft[int, int](i)
		}
		z := 0
		e.WhenLeft(func(x int) { z += x })
		e.WhenRight(func(x int) { z += x })

		return z != 2*i && e.IsLeft() != e.IsRight()
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPropSwapEitherSelfInverting(t *testing.T) {
	f := func(i int, s string, isRight bool) bool {
		var e Either[int, string]
		if isRight {
			e = NewRight[int, string](s)
		} else {
			e = NewLeft[int, string](i)
		}
		return e.Swap().Swap() == e
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPropMapLeftIdentity(t *testing.T) {
	f := func(i int, s string, isRight bool) bool {
		var e Either[int, string]
		if isRight {
			e = NewRight[int, string](s)
		} else {
			e = NewLeft[int, string](i)
		}
		return MapLeft[int, string, int](Iden[int])(e) == e
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPropMapRightIdentity(t *testing.T) {
	f := func(i int, s string, isRight bool) bool {
		var e Either[int, string]
		if isRight {
			e = NewRight[int, string](s)
		} else {
			e = NewLeft[int, string](i)
		}
		return MapRight[int, string, string](Iden[string])(e) == e
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPropToOptionIdentities(t *testing.T) {
	f := func(i int, s string, isRight bool) bool {
		var e Either[int, string]
		if isRight {
			e = NewRight[int, string](s)

			r2O := e.RightToSome() == Some(s)
			o2R := e == SomeToRight(Some(s), i)
			l2O := e.LeftToSome() == None[int]()

			return r2O && o2R && l2O
		} else {
			e = NewLeft[int, string](i)
			l2O := e.LeftToSome() == Some(i)
			o2L := e == SomeToLeft(Some(i), s)
			r2O := e.RightToSome() == None[string]()

			return l2O && o2L && r2O
		}
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPropUnwrapIdentities(t *testing.T) {
	f := func(i int, s string, isRight bool) bool {
		var e Either[int, string]
		if isRight {
			e = NewRight[int, string](s)
			return e.UnwrapRightOr("") == s &&
				e.UnwrapLeftOr(0) == 0
		} else {
			e = NewLeft[int, string](i)
			return e.UnwrapLeftOr(0) == i &&
				e.UnwrapRightOr("") == ""
		}
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}
