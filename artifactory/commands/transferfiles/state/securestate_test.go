//go:build !windows

package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepoStateSymlinkNotFollowed(t *testing.T) {
	stateManager, cleanUp := InitStateTest(t)
	defer cleanUp()
	require.NoError(t, stateManager.SetRepoState(repo1Key, 0, 0, false, true))

	statePath, err := GetRepoStateFilepath(repo1Key, false)
	require.NoError(t, err)
	victim := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.Remove(statePath))
	require.NoError(t, os.Symlink(victim, statePath))

	// A planted symlink must not be loaded.
	_, _, err = LoadTransferState(repo1Key, false)
	assert.Error(t, err)

	// Writing the state must replace the link and never create the link target.
	require.NoError(t, stateManager.persistTransferState(false))
	_, err = os.Lstat(victim)
	assert.True(t, os.IsNotExist(err))
	info, err := os.Lstat(statePath)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())

	// Positive control: the legitimately written state loads.
	_, exists, err := LoadTransferState(repo1Key, false)
	assert.NoError(t, err)
	assert.True(t, exists)
}
