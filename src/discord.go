package main

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

var (
	discord               *discordgo.Session
	discordToken          string
	discordListenChannels []string
	discordLobbyChannel   string
	discordBotChannel     string
	discordBotID          string
	discordLastAtHere     time.Time
)

/*
	Initialization functions
*/

func discordInit() {
	// Read some configuration values from environment variables
	// (they were loaded from the .env file in main.go)
	discordToken = os.Getenv("DISCORD_TOKEN")
	if len(discordToken) == 0 {
		log.Info("The \"DISCORD_TOKEN\" environment variable is blank; aborting Discord initialization.")
		return
	}
	discordListenChannelsString := os.Getenv("DISCORD_LISTEN_CHANNEL_IDS")
	if len(discordListenChannelsString) == 0 {
		log.Info("The \"DISCORD_LISTEN_CHANNEL_IDS\" environment variable is blank; aborting Discord initialization.")
		return
	}
	discordListenChannels = strings.Split(discordListenChannelsString, ",")
	discordLobbyChannel = os.Getenv("DISCORD_LOBBY_CHANNEL_ID")
	if len(discordLobbyChannel) == 0 {
		log.Info("The \"DISCORD_LOBBY_CHANNEL_ID\" environment variable is blank; aborting Discord initialization.")
		return
	}
	discordBotChannel = os.Getenv("DISCORD_BOT_CHANNEL_ID")
	if len(discordBotChannel) == 0 {
		log.Info("The \"DISCORD_BOT_CHANNEL_ID\" environment variable is blank; aborting Discord initialization.")
		return
	}

	// Get the last time a "@here" ping was sent
	var timeAsString string
	if v, err := db.DiscordMetadata.Get("last_at_here"); err != nil {
		log.Fatal("Failed to retrieve the \"last_at_here\" value from the database:", err)
	} else {
		timeAsString = v
	}
	if v, err := time.Parse(time.RFC3339, timeAsString); err != nil {
		log.Fatal("Failed to parse the \"last_at_here\" value from the database:", err)
	} else {
		discordLastAtHere = v
	}

	// Start the Discord bot in a new goroutine
	go discordConnect()
}

func discordConnect() {
	// Bot accounts must be prefixed with "Bot"
	if v, err := discordgo.New("Bot " + discordToken); err != nil {
		log.Error("Failed to create a Discord session:", err)
		return
	} else {
		discord = v
	}

	// Register function handlers for various events
	discord.AddHandler(discordReady)
	discord.AddHandler(discordMessageCreate)

	// Open the websocket and begin listening
	if err := discord.Open(); err != nil {
		log.Error("Failed to open the Discord session:", err)
		return
	}

	// Announce that the server has started
	msg := "The server has successfully started at: " + time.Now().Format("Mon Jan 02 15:04:05 MST 2006")
	commandChat(nil, &CommandData{
		Msg:    msg,
		Room:   "lobby",
		Server: true,
		Spam:   true,
	})
}

/*
	Event handlers
*/

func discordReady(s *discordgo.Session, event *discordgo.Ready) {
	log.Info("Discord bot connected with username: " + event.User.Username)
	discordBotID = event.User.ID
}

// Copy messages from Discord to the lobby
func discordMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore all messages created by the bot itself
	if m.Author.ID == discordBotID {
		return
	}

	// Only replicate messages from the listed channels
	if !stringInSlice(m.ChannelID, discordListenChannels) {
		return
	}

	// Get the channel
	var channel *discordgo.Channel
	if v, err := discord.Channel(m.ChannelID); err != nil {
		log.Error("Failed to get the Discord channel of \""+m.ChannelID+"\":", err)
		return
	} else {
		channel = v
	}

	// Log the message
	log.Info("[D#" + channel.Name + "] <" + m.Author.Username + "#" + m.Author.Discriminator + "> " + m.Content)

	// Send everyone the notification
	commandMutex.Lock()
	defer commandMutex.Unlock()
	commandChat(nil, &CommandData{
		Username:             discordGetNickname(m.Author.ID),
		Msg:                  m.Content,
		Discord:              true,
		Room:                 "lobby",
		DiscordID:            m.Author.ID,            // Pass through the ID in case we need it for a custom command
		DiscordDiscriminator: m.Author.Discriminator, // Pass through the discriminator so we can append it to the username
	})
}

/*
	Miscellaneous functions
*/

func discordSend(to string, username string, msg string) {
	if discord == nil {
		return
	}

	// Make a message prefix to identify the user
	var fullMsg string
	if username != "" {
		// Text inside double asterisks are bolded
		fullMsg += "<**" + username + "**> "
	}
	fullMsg += msg

	if _, err := discord.ChannelMessageSend(to, fullMsg); err != nil {
		log.Error("Failed to send \""+fullMsg+"\" to Discord:", err)
		return
	}
}

func discordGetNickname(discordID string) string {
	// Get the Discord guild object
	var guild *discordgo.Guild
	if v, err := discord.Guild(discordListenChannels[0]); err != nil {
		log.Error("Failed to get the Discord guild:", err)
		return "[error]"
	} else {
		guild = v
	}
	// (assume that the first channel ID is the same as the server ID)

	// Get their custom nickname for the Discord server, if any
	log.Debug("Searching for Discord ID: " + discordID + ", " +
		"len(guild.Members) = " + strconv.Itoa(len(guild.Members)) + ", " +
		"guild.MemberCount = " + strconv.Itoa(guild.MemberCount))
	for i, member := range guild.Members {
		log.Debug("Member " + strconv.Itoa(i) + ": " + member.User.ID + " - " + member.Nick + " - " + member.User.Username)
		if member.User.ID != discordID {
			continue
		}

		if member.Nick == "" {
			return member.User.Username
		}

		return member.Nick
	}

	// There is a bug in the Discord API where "guild.MemberCount" will not contain all of the members of the server
	// As a last resort, get the Discord user object directly
	if user, err := discord.User(discordID); err != nil {
		log.Error("Failed to get the Discord user of \""+discordID+"\":", err)
		return "[error]"
	} else {
		return user.Username
	}
}

func discordGetChannel(discordID string) string {
	// Get the Discord guild object
	var guild *discordgo.Guild
	if v, err := discord.Guild(discordListenChannels[0]); err != nil {
		log.Error("Failed to get the Discord guild:", err)
		return ""
	} else {
		guild = v
	}
	// (assume that the first channel ID is the same as the server ID)

	// Get the name of the channel
	for _, channel := range guild.Channels {
		if channel.ID == discordID {
			return channel.Name
		}
	}

	return "Unknown Discord Channel"
}

func discordGetID(username string) string {
	// Get the Discord guild object
	var guild *discordgo.Guild
	if v, err := discord.Guild(discordListenChannels[0]); err != nil {
		log.Error("Failed to get the Discord guild:", err)
		return ""
	} else {
		guild = v
	}
	// (assume that the first channel ID is the same as the server ID)

	// Find the ID that corresponds to this username
	for _, member := range guild.Members {
		if member.Nick == username || member.User.Username == username {
			return member.User.ID
		}
	}

	return ""
}
