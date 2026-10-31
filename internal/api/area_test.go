// SPDX-License-Identifier: AGPL-3.0-only

package api

// Unit test (no database) for areaStoreSetError: the two sentinels #442
// increment 3 added (store.ErrAreaNameTaken, store.ErrAreaLabelTaken) must
// each map to exactly ONE JMAP property, not the pre-increment-3 fallback of
// naming both.

import (
	"errors"
	"fmt"
	"testing"

	"mailroom/internal/store"
)

func TestAreaStoreSetErrorMapsNameCollisionToNameAlone(t *testing.T) {
	err := fmt.Errorf("%w: %q", store.ErrAreaNameTaken, "Receipts")
	serr := areaStoreSetError(err)
	if len(serr.Properties) != 1 || serr.Properties[0] != "name" {
		t.Errorf("Properties = %v, want [name]", serr.Properties)
	}
}

func TestAreaStoreSetErrorMapsLabelCollisionToLabelIDAlone(t *testing.T) {
	err := fmt.Errorf("%w: %s", store.ErrAreaLabelTaken, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	serr := areaStoreSetError(err)
	if len(serr.Properties) != 1 || serr.Properties[0] != "labelId" {
		t.Errorf("Properties = %v, want [labelId]", serr.Properties)
	}
}

// TestAreaStoreSetErrorFeedTakenStillSniffsTabVsLabel guards the
// pre-existing ErrFeedTaken behaviour: increment 3 must not have disturbed
// it while adding the two new sentinels above it.
func TestAreaStoreSetErrorFeedTakenStillSniffsTabVsLabel(t *testing.T) {
	tabErr := fmt.Errorf(`%w: tab "updates" already feeds %q`, store.ErrFeedTaken, "News")
	if serr := areaStoreSetError(tabErr); len(serr.Properties) != 1 || serr.Properties[0] != "tabs" {
		t.Errorf("tab feed conflict: Properties = %v, want [tabs]", serr.Properties)
	}
	labelErr := fmt.Errorf("%w: label %s already feeds %q", store.ErrFeedTaken, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "News")
	if serr := areaStoreSetError(labelErr); len(serr.Properties) != 1 || serr.Properties[0] != "labelIds" {
		t.Errorf("label feed conflict: Properties = %v, want [labelIds]", serr.Properties)
	}
}

// TestAreaStoreSetErrorFallsBackToBothPropertiesForAnythingElse: an
// unrecognised store error still names both name and labelId, since there is
// no sentinel to tell which one was at fault.
func TestAreaStoreSetErrorFallsBackToBothPropertiesForAnythingElse(t *testing.T) {
	serr := areaStoreSetError(errors.New("store: some other failure"))
	if len(serr.Properties) != 2 || serr.Properties[0] != "name" || serr.Properties[1] != "labelId" {
		t.Errorf("Properties = %v, want [name labelId]", serr.Properties)
	}
}
