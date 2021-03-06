package main

import (
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Table describes the container that a player can join, whether it is an unstarted game,
// an ongoing game, a solo replay, a shared replay, etc.
// A tag of `json:"-"` denotes that the JSON serializer should skip the field when serializing
type Table struct {
	ID          uint64
	Name        string
	InitialName string // The name of the table before it was converted to a replay

	Players    []*Player
	Spectators []*Spectator `json:"-"`
	// We keep track of players who have been kicked from the game
	// so that we can prevent them from rejoining
	KickedPlayers map[int]struct{} `json:"-"`
	// We also keep track of spectators who have disconnected
	// so that we can automatically put them back into the shared replay
	DisconSpectators map[int]struct{} `json:"-"`

	// This is the user ID of the person who started the table
	// or the current leader of the shared replay
	Owner   int
	Visible bool // Whether or not this table is shown to other users
	// This is an Argon2id hash generated from the plain-text password
	// that the table creator sends us
	PasswordHash   string
	Running        bool
	Replay         bool
	AutomaticStart int // See "chatTable.go"
	Progress       int // Displayed as a percentage on the main lobby screen

	DatetimeCreated      time.Time
	DatetimeLastJoined   time.Time
	DatetimePlannedStart time.Time
	// This is updated any time a player interacts with the game / replay
	// (used to determine when a game is idle)
	DatetimeLastAction time.Time

	// All of the game state is contained within the "Game" object
	Game *Game

	// The variant and other game settings are contained within the "Options" object
	Options      *Options      // Options that are stored in the database
	ExtraOptions *ExtraOptions // Options that are not stored in the database

	Chat     []*TableChatMessage // All of the in-game chat history
	ChatRead map[int]int         // A map of which users have read which messages
	Deleted  bool                `json:"-"` // Used to prevent race conditions

	// Each table has its own mutex to ensure that only one action can occur at the same time
	mutex *sync.Mutex
}

type TableChatMessage struct {
	UserID   int
	Username string
	Msg      string
	Datetime time.Time
	Server   bool
}

var (
	// The counter is atomically incremented before assignment,
	// so the first table ID will be 1 and will increase from there
	tableIDCounter uint64 = 0
)

func NewTable(name string, owner int) *Table {
	// Create the table object
	return &Table{
		ID:          getNewTableID(),
		Name:        name,
		InitialName: "", // This must stay blank in shared replays

		Players:          make([]*Player, 0),
		Spectators:       make([]*Spectator, 0),
		KickedPlayers:    make(map[int]struct{}),
		DisconSpectators: make(map[int]struct{}),

		Owner:          owner,
		Visible:        true, // Tables are visible by default
		PasswordHash:   "",
		Running:        false,
		Replay:         false,
		AutomaticStart: 0,
		Progress:       0,

		DatetimeCreated:      time.Now(),
		DatetimeLastJoined:   time.Time{},
		DatetimePlannedStart: time.Time{},
		DatetimeLastAction:   time.Time{},

		Game: nil,

		Options:      NewOptions(),
		ExtraOptions: &ExtraOptions{},

		Chat:     make([]*TableChatMessage, 0),
		ChatRead: make(map[int]int),
		Deleted:  false,

		mutex: &sync.Mutex{},
	}
}

func getNewTableID() uint64 {
	tableIDs := tables.GetKeys()

	for {
		newTableID := atomic.AddUint64(&tableIDCounter, 1)

		// Ensure that the table ID does not conflict with any existing tables
		valid := true
		for _, tableID := range tableIDs {
			if tableID == newTableID {
				valid = false
				break
			}
		}
		if valid {
			return newTableID
		}
	}
}

func (t *Table) Lock() {
	logger.Debug("ACQUIRING table " + strconv.FormatUint(t.ID, 10) + " lock.")
	debug.PrintStack()
	t.mutex.Lock()
	logger.Debug("ACQUIRED table " + strconv.FormatUint(t.ID, 10) + " lock.")
	debug.PrintStack()
}

func (t *Table) Unlock() {
	logger.Debug("RELEASING table " + strconv.FormatUint(t.ID, 10) + " lock.")
	debug.PrintStack()
	t.mutex.Unlock()
}

// CheckIdle is meant to be called in a new goroutine
func (t *Table) CheckIdle() {
	// Disable idle timeouts in development
	if isDev {
		return
	}

	// Set the last action
	t.Lock()
	t.DatetimeLastAction = time.Now()
	t.Unlock()

	// We want to clean up idle games, so sleep for a reasonable amount of time
	time.Sleep(IdleGameTimeout)

	// Check to see if the table still exists
	t2, exists := getTableAndLock(nil, t.ID, false)
	if !exists || t != t2 {
		return
	}
	t.Lock()
	defer t.Unlock()

	// Don't do anything if there has been an action in the meantime
	if time.Since(t.DatetimeLastAction) < IdleGameTimeout {
		return
	}

	t.EndIdle()
}

// EndIdle is called when a table has been idle for a while and should be automatically ended
func (t *Table) EndIdle() {
	logger.Info(t.GetName() + " Idle timeout has elapsed; ending the game.")

	if t.Replay {
		// If this is a replay,
		// we want to send a message to the client that will take them back to the lobby
		t.NotifyBoot()
	}

	// Boot all of the spectators, if any
	for len(t.Spectators) > 0 {
		sp := t.Spectators[0]
		s := sp.Session
		if s == nil {
			// A spectator's session should never be nil
			// They might be in the process of reconnecting,
			// so make a fake session that will represent them
			s = NewFakeSession(sp.ID, sp.Name)
			logger.Info("Created a new fake session in the \"CheckIdle()\" function.")
		}
		commandTableUnattend(s, &CommandData{ // nolint: exhaustivestruct
			TableID: t.ID,
			NoLock:  true,
		})
	}

	if t.Replay {
		// If this is a replay, then we are done;
		// it should automatically end now that all of the spectators have left
		return
	}

	s := t.GetOwnerSession()
	if t.Running {
		// We need to end a game that has started
		// (this will put everyone in a non-shared replay of the idle game)
		commandAction(s, &CommandData{ // nolint: exhaustivestruct
			TableID: t.ID,
			Type:    ActionTypeEndGame,
			Target:  -1,
			Value:   EndConditionIdleTimeout,
			NoLock:  true,
		})
	} else {
		// We need to end a game that has not started yet
		// Force the owner to leave, which should subsequently eject everyone else
		// (this will send everyone back to the main lobby screen)
		commandTableLeave(s, &CommandData{ // nolint: exhaustivestruct
			TableID: t.ID,
			NoLock:  true,
		})
	}
}

func (t *Table) GetName() string {
	g := t.Game
	name := "Table #" + strconv.FormatUint(t.ID, 10) + " (" + t.Name + ") - "
	if g == nil {
		name += "Not started"
	} else {
		name += "Turn " + strconv.Itoa(g.Turn)
	}
	name += " - "
	return name
}

func (t *Table) GetRoomName() string {
	// Various places in the code base check for room names with a prefix of "table"
	return "table" + strconv.FormatUint(t.ID, 10)
}

func (t *Table) GetPlayerIndexFromID(userID int) int {
	for i, p := range t.Players {
		if p.ID == userID {
			return i
		}
	}
	return -1
}

func (t *Table) GetSpectatorIndexFromID(userID int) int {
	for i, sp := range t.Spectators {
		if sp.ID == userID {
			return i
		}
	}
	return -1
}

func (t *Table) GetOwnerSession() *Session {
	if t.Replay {
		logger.Error("The \"GetOwnerSession\" function was called on a table that is a replay.")
		return nil
	}

	var s *Session
	for _, p := range t.Players {
		if p.ID == t.Owner {
			s = p.Session
			if s == nil {
				// A player's session should never be nil
				// They might be in the process of reconnecting,
				// so make a fake session that will represent them
				s = NewFakeSession(p.ID, p.Name)
				logger.Info("Created a new fake session in the \"GetOwnerSession()\" function.")
			}
			break
		}
	}

	if s == nil {
		logger.Error("Failed to find the owner for table " + strconv.FormatUint(t.ID, 10) + ".")
		s = NewFakeSession(-1, "Unknown")
		logger.Info("Created a new fake session in the \"GetOwnerSession()\" function.")
	}

	return s
}

// Get a list of online user sessions that should be notified about actions and other important
// events from this table
// We do not want to notify everyone about every table, as that would constitute a lot of spam
// Only notify:
// 1) players who are currently in the game
// 2) users that have players or spectators in this table on their friends list
func (t *Table) GetNotifySessions(excludePlayers bool) []*Session {
	// First, make a map that contains a list of every relevant user
	notifyMap := make(map[int]struct{})

	if !t.Replay {
		for _, p := range t.Players {
			if p.Session == nil {
				continue
			}
			notifyMap[p.ID] = struct{}{}
			for userID := range p.Session.ReverseFriends() {
				notifyMap[userID] = struct{}{}
			}
		}
	}

	for _, sp := range t.Spectators {
		if sp.Session == nil {
			continue
		}
		notifyMap[sp.ID] = struct{}{}
		for userID := range sp.Session.ReverseFriends() {
			notifyMap[userID] = struct{}{}
		}
	}

	// In some situations, we need to only notify the reverse friends;
	// including the players would mean that the players get duplicate messages
	if excludePlayers {
		for _, p := range t.Players {
			delete(notifyMap, p.ID)
		}
	}

	// Go through the map and build a list of users that happen to be currently online
	notifySessions := make([]*Session, 0)
	for userID := range notifyMap {
		if s, ok := sessions.Get(userID); ok {
			notifySessions = append(notifySessions, s)
		}
	}

	return notifySessions
}

func (t *Table) GetSharedReplayLeaderName() string {
	// Get the username of the game owner
	// (the "Owner" field is used to store the leader of the shared replay)
	for _, sp := range t.Spectators {
		if sp.ID == t.Owner {
			return sp.Name
		}
	}

	// The leader is not currently present,
	// so try getting their username from the players object
	for _, p := range t.Players {
		if p.ID == t.Owner {
			return p.Name
		}
	}

	// The leader is not currently present and was not a member of the original game,
	// so we need to look up their username from the database
	if v, err := models.Users.GetUsername(t.Owner); err != nil {
		logger.Error("Failed to get the username for user "+strconv.Itoa(t.Owner)+
			" who is the owner of table:", t.ID)
		return "(Unknown)"
	} else {
		return v
	}
}
