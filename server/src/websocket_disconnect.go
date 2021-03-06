package main

import (
	"strconv"

	melody "gopkg.in/olahol/melody.v1"
)

func websocketDisconnect(ms *melody.Session) {
	s := getSessionFromMelodySession(ms)
	if s == nil {
		return
	}

	logger.Info("Entered the \"websocketDisconnect()\" function for user: " + s.Username)

	// We only want one computer to connect to one user at a time
	// Use a dedicated mutex to prevent race conditions
	sessions.ConnectMutex.Lock()
	defer sessions.ConnectMutex.Unlock()

	websocketDisconnectRemoveFromMap(s)
	websocketDisconnectRemoveFromGames(s)

	// Alert everyone that a user has logged out
	notifyAllUserLeft(s)
}

func websocketDisconnectRemoveFromMap(s *Session) {
	sessions.Delete(s.UserID)
	logger.Info("User \"" + s.Username + "\" disconnected;" + strconv.Itoa(sessions.Length()) + " user(s) now connected.")
}

func websocketDisconnectRemoveFromGames(s *Session) {
	// Look for the disconnecting player in all of the tables
	ongoingGameTableIDs := make([]uint64, 0)
	preGameTableIDs := make([]uint64, 0)
	spectatingTableIDs := make([]uint64, 0)

	tableList := tables.GetList()
	for _, t := range tableList {
		t.Lock()

		// They could be one of the players (1/2)
		playerIndex := t.GetPlayerIndexFromID(s.UserID)
		if playerIndex != -1 && !t.Replay {
			if t.Running {
				ongoingGameTableIDs = append(ongoingGameTableIDs, t.ID)
			} else {
				preGameTableIDs = append(preGameTableIDs, t.ID)
			}
		}

		// They could be one of the spectators (2/2)
		spectatorIndex := t.GetSpectatorIndexFromID(s.UserID)
		if spectatorIndex != -1 {
			spectatingTableIDs = append(spectatingTableIDs, t.ID)
		}

		t.Unlock()
	}

	for _, ongoingGameTableID := range ongoingGameTableIDs {
		logger.Info("Unattending player \"" + s.Username + "\" from ongoing table " +
			strconv.FormatUint(ongoingGameTableID, 10) + " since they disconnected.")
		commandTableUnattend(s, &CommandData{ // nolint: exhaustivestruct
			TableID: ongoingGameTableID,
		})
	}

	for _, preGameTableID := range preGameTableIDs {
		logger.Info("Ejecting player \"" + s.Username + "\" from unstarted table " +
			strconv.FormatUint(preGameTableID, 10) + " since they disconnected.")
		commandTableLeave(s, &CommandData{ // nolint: exhaustivestruct
			TableID: preGameTableID,
		})
	}

	for _, spectatingTableID := range spectatingTableIDs {
		logger.Info("Ejecting spectator \"" + s.Username + "\" from table " +
			strconv.FormatUint(spectatingTableID, 10) + " since they disconnected.")
		commandTableUnattend(s, &CommandData{ // nolint: exhaustivestruct
			TableID: spectatingTableID,
		})

		// Additionally, we also want to add this user to the map of disconnected spectators
		// (so that they will be automatically reconnected to the game if/when they reconnect)
		t, exists := getTableAndLock(s, spectatingTableID, true)
		if exists {
			t.DisconSpectators[s.UserID] = struct{}{}
			t.Unlock()
		}
	}
}
