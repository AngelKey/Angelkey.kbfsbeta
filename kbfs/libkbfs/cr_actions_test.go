package libkbfs

import (
	"reflect"
	"testing"
)

func TestCRActionsCollapseNoChange(t *testing.T) {
	al := crActionList{
		&copyUnmergedEntryAction{"old1", "new1", "", false, false,
			DirEntry{}, nil},
		&copyUnmergedEntryAction{"old2", "new2", "", false, false,
			DirEntry{}, nil},
		&renameUnmergedAction{"old3", "new3", ""},
		&renameMergedAction{"old4", "new4", "", BlockPointer{}},
		&copyUnmergedAttrAction{"old5", "new5", []attrChange{mtimeAttr}},
	}

	newList := al.collapse()
	if !reflect.DeepEqual(al, newList) {
		t.Errorf("Collapse returned different list: %v vs %v", al, newList)
	}
}

func TestCRActionsCollapseEntry(t *testing.T) {
	al := crActionList{
		&copyUnmergedAttrAction{"old", "new", []attrChange{mtimeAttr}},
		&copyUnmergedEntryAction{"old", "new", "", false, false,
			DirEntry{}, nil},
		&renameUnmergedAction{"old", "new", ""},
	}

	expected := crActionList{
		al[2],
	}

	newList := al.collapse()
	if !reflect.DeepEqual(expected, newList) {
		t.Errorf("Collapse returned unexpected list: %v vs %v",
			expected, newList)
	}

	// change the order
	al = crActionList{al[1], al[2], al[0]}

	newList = al.collapse()
	if !reflect.DeepEqual(expected, newList) {
		t.Errorf("Collapse returned unexpected list: %v vs %v",
			expected, newList)
	}

	// Omit the top action this time
	al = crActionList{al[0], al[2]}
	expected = crActionList{al[0]}

	newList = al.collapse()
	if !reflect.DeepEqual(expected, newList) {
		t.Errorf("Collapse returned unexpected list: %v vs %v",
			expected, newList)
	}
}
func TestCRActionsCollapseAttr(t *testing.T) {
	al := crActionList{
		&copyUnmergedAttrAction{"old", "new", []attrChange{mtimeAttr}},
		&copyUnmergedAttrAction{"old", "new", []attrChange{exAttr}},
		&copyUnmergedAttrAction{"old", "new", []attrChange{mtimeAttr}},
	}

	expected := crActionList{
		&copyUnmergedAttrAction{"old", "new", []attrChange{mtimeAttr, exAttr}},
	}

	newList := al.collapse()
	if !reflect.DeepEqual(expected, newList) {
		t.Errorf("Collapse returned unexpected list: %v vs %v",
			expected, newList)
	}
}
