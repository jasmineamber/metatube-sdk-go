package javdb

import (
	"testing"

	"github.com/metatube-community/metatube-sdk-go/provider/internal/testkit"
)

func TestJavDB_GetMovieInfoByID(t *testing.T) {
	testkit.Test(t, New, []string{
		"Yw8XB",
	})
}

func TestJavBus_SearchMovie(t *testing.T) {
	testkit.Test(t, New, []string{
		"ARM-383",
	})
}
