package irckit

import (
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/nlopes/slack"
)

type SlackInfo struct {
	Token     string
	sc        *slack.Client
	rtm       *slack.RTM
	sinfo     *slack.Info
	susers    map[string]slack.User
	connected bool
}

func (u *User) loginToSlack() (*slack.Client, error) {
	u.sc = slack.New(u.Token)
	u.rtm = u.sc.NewRTM()
	u.susers = make(map[string]slack.User)
	go u.rtm.ManageConnection()
	//time.Sleep(time.Second * 2)
	u.sinfo = u.rtm.GetInfo()
	for u.sinfo == nil {
		time.Sleep(time.Millisecond * 500)
		logger.Debug("still waiting for sinfo")
		u.sinfo = u.rtm.GetInfo()
	}
	go u.handleSlack()
	u.addSlackUsersToChannels()
	u.connected = true
	return u.sc, nil
}

func (u *User) logoutFromSlack() error {
	logger.Debug("calling logout from slack")
	err := u.rtm.Disconnect()
	if err != nil {
		logger.Debug("logoutfrom slack", err)
		return err
	}
	u.Srv.Logout(u)
	u.sc = nil
	logger.Info("logout succeeded")
	u.connected = false
	return nil
}

func (u *User) createSlackUser(slackuser *slack.User) *User {
	if slackuser == nil {
		return nil
	}
	if ghost, ok := u.Srv.HasUser(slackuser.Name); ok {
		return ghost
	}
	ghost := &User{Nick: slackuser.Name, User: slackuser.ID, Real: slackuser.RealName, Host: "host", Roles: "", channels: map[Channel]struct{}{}}
	ghost.MmGhostUser = true
	u.Srv.Add(ghost)
	return ghost
}

func (u *User) addSlackUserToChannel(user *slack.User, channel string, channelId string) {
	if user == nil {
		return
	}
	ghost := u.createSlackUser(user)
	if ghost == nil {
		logger.Warnf("Cannot join %v into %s", user, channel)
		return
	}
	logger.Debugf("adding %s to %s (%s)", ghost.Nick, channel, channelId)
	ch := u.Srv.Channel(channelId)
	logger.Debugf("channel: %#v %#v", ch.String(), ch.ID())
	ch.Join(ghost)
}

func (u *User) addSlackUsersToChannels() {
	srv := u.Srv
	throttle := time.Tick(time.Millisecond * 100)
	logger.Debug("in addUsersToChannels()")
	// add all users, also who are not on channels
	ch := srv.Channel("&users")
	users, _ := u.sc.GetUsers()
	for _, mmuser := range users {
		// do not add our own nick
		if mmuser.ID == u.sinfo.User.ID {
			continue
		}
		u.createSlackUser(&mmuser)
		u.addSlackUserToChannel(&mmuser, "&users", "&users")
		u.susers[mmuser.ID] = mmuser
	}
	ch.Join(u)

	channels := make(chan interface{}, 10)
	for i := 0; i < 10; i++ {
		go u.addSlackUserToChannelWorker(channels, throttle)
	}
	groups, _ := u.sc.GetGroups(true)
	mmchannels, _ := u.sc.GetChannels(true)
	for _, mmchannel := range mmchannels {
		if mmchannel.IsMember {
			logger.Debug("Adding channel", mmchannel)
			channels <- mmchannel
		}
	}
	for _, mmchannel := range groups {
		logger.Debug("Adding private channel", mmchannel)
		channels <- mmchannel
	}
	close(channels)
}

func (u *User) addSlackUserToChannelWorker(channels <-chan interface{}, throttle <-chan time.Time) {
	var ID, name string
	for {
		mmchannel, ok := <-channels
		if !ok {
			logger.Debug("Done adding user to channels")
			return
		}
		<-throttle
		switch mmchannel.(type) {
		case slack.Channel:
			ID = mmchannel.(slack.Channel).ID
			name = mmchannel.(slack.Channel).Name
			u.syncSlackChannel(ID, name)
		case slack.Group:
			ID = mmchannel.(slack.Group).ID
			name = mmchannel.(slack.Group).Name
			logger.Debugf("GROUP %#v", mmchannel.(slack.Group))
			u.syncSlackGroup(ID, name)

		}
		// exclude direct messages
		//var spoof func(string, string)
		//ch := u.Srv.Channel(mmchannel.ID)
		// post everything to the channel you haven't seen yet
	}
}

func (u *User) handleSlack() {
	for {
		/*
			if u.mc.WsQuit {
				logger.Debug("exiting handleWsMessage")
				return
			}
		*/
		logger.Debug("in handleSlack")
		for msg := range u.rtm.IncomingEvents {
			switch ev := msg.Data.(type) {
			case *slack.MessageEvent:
				u.handleSlackActionPost(ev)
			case *slack.DisconnectedEvent:
				logger.Debugf("disconnecting..")
				return
			}
		}
	}
	/*
			logger.Debugf("MMUser WsReceiver: %#v", message.Raw)
			// check if we have the users/channels in our cache. If not update
			u.checkWsActionMessage(message.Raw, updateChannelsThrottle)
			switch message.Raw.Event {
			case model.WEBSOCKET_EVENT_POSTED:
				u.handleWsActionPost(message.Raw)
			case model.WEBSOCKET_EVENT_POST_EDITED:
				u.handleWsActionPost(message.Raw)
			case model.WEBSOCKET_EVENT_USER_REMOVED:
				u.handleWsActionUserRemoved(message.Raw)
			case model.WEBSOCKET_EVENT_USER_ADDED:
				u.handleWsActionUserAdded(message.Raw)
			}
		}
	*/
}

func (u *User) handleSlackActionPost(rmsg *slack.MessageEvent) {
	var ch Channel
	logger.Debugf("handleSlackActionPost() receiving msg %#v", rmsg)
	if len(rmsg.Attachments) > 0 {
		// skip messages we made ourselves
		if rmsg.Attachments[0].CallbackID == "matterircd" {
			return
		}
	}

	user, err := u.rtm.GetUserInfo(rmsg.User)
	if err != nil {
		return
	}

	// handle bot messages
	botname := ""
	if rmsg.User == "" && rmsg.BotID != "" {
		bot, _ := u.rtm.GetBotInfo(rmsg.BotID)
		if bot.Name != "" {
			botname = bot.Name
			if rmsg.Username != "" {
				botname = rmsg.Username
			}
		}
	}

	// create new "ghost" user
	ghost := u.createSlackUser(user)

	spoofUsername := user.ID
	if ghost != nil {
		spoofUsername = ghost.Nick
	}

	// if we have a botname, use it
	if botname != "" {
		spoofUsername = botname
	}

	msgs := strings.Split(rmsg.Text, "\n")
	// direct message

	if ghost != nil {
		ch = u.Srv.Channel(rmsg.Channel)
		// join if not in channel
		if !ch.HasUser(ghost) {
			ch.Join(ghost)
		}
	}

	for _, m := range msgs {
		// cleanup the message
		m = u.replaceMention(m)
		m = u.replaceVariable(m)
		m = u.replaceChannel(m)
		m = u.replaceURL(m)
		m = html.UnescapeString(m)

		// look in attachments if we have no text
		if m == "" {
			for _, attach := range rmsg.Attachments {
				if attach.Text != "" {
					m = attach.Text
				} else {
					m = attach.Fallback
				}
			}
		}

		// still no text, ignore this message
		if m == "" {
			continue
		}

		if strings.HasPrefix(rmsg.Channel, "D") {
			u.MsgSpoofUser(spoofUsername, m)
		} else {
			ch.SpoofMessage(spoofUsername, m)
		}
	}
}

// sync IRC with mattermost channel state
func (u *User) syncSlackChannel(id string, name string) {
	srv := u.Srv
	info, err := u.sc.GetChannelInfo(id)
	if err != nil {
		logger.Info(err)
	}

	for _, user := range info.Members {
		if u.sinfo.User.ID != user {
			//slackuser, _ := u.sc.GetUserInfo(user)
			if slackuser, ok := u.susers[user]; ok {
				u.addSlackUserToChannel(&slackuser, "#"+name, id)
			}
		}
	}
	// before joining ourself
	for _, user := range info.Members {
		// join all the channels we're on on MM
		if user == u.sinfo.User.ID {
			ch := srv.Channel(id)
			// only join when we're not yet on the channel
			if !ch.HasUser(u) {
				logger.Debugf("syncSlackchannel adding myself to %s (id: %s)", name, id)
				ch.Join(u)
				//ch.Topic(u, u.mc.GetChannelHeader(id))
			}
			break
		}
	}
}

// sync IRC with mattermost channel state
func (u *User) syncSlackGroup(id string, name string) {
	srv := u.Srv
	info, err := u.sc.GetGroupInfo(id)
	if err != nil {
		logger.Info(err)
	}

	for _, user := range info.Members {
		if u.sinfo.User.ID != user {
			//slackuser, _ := u.sc.GetUserInfo(user)
			if slackuser, ok := u.susers[user]; ok {
				u.addSlackUserToChannel(&slackuser, "#"+name, id)
			}
		}
	}
	// before joining ourself
	for _, user := range info.Members {
		// join all the channels we're on on MM
		if user == u.sinfo.User.ID {
			ch := srv.Channel(id)
			// only join when we're not yet on the channel
			if !ch.HasUser(u) {
				logger.Debugf("syncSlackgroup adding myself to %s (id: %s)", name, id)
				ch.Join(u)
				//ch.Topic(u, u.mc.GetChannelHeader(id))
			}
			break
		}
	}
}

// @see https://api.slack.com/docs/message-formatting#linking_to_channels_and_users
func (u *User) replaceMention(text string) string {
	results := regexp.MustCompile(`<@([a-zA-z0-9]+)>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		text = strings.Replace(text, "<@"+r[1]+">", "@"+u.userName(r[1]), -1)
	}
	return text
}

// @see https://api.slack.com/docs/message-formatting#linking_to_channels_and_users
func (u *User) replaceChannel(text string) string {
	results := regexp.MustCompile(`<#[a-zA-Z0-9]+\|(.+?)>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		text = strings.Replace(text, r[0], "#"+r[1], -1)
	}
	return text
}

// @see https://api.slack.com/docs/message-formatting#variables
func (u *User) replaceVariable(text string) string {
	results := regexp.MustCompile(`<!((?:subteam\^)?[a-zA-Z0-9]+)(?:\|@?(.+?))?>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		if r[2] != "" {
			text = strings.Replace(text, r[0], "@"+r[2], -1)
		} else {
			text = strings.Replace(text, r[0], "@"+r[1], -1)
		}
	}
	return text
}

// @see https://api.slack.com/docs/message-formatting#linking_to_urls
func (u *User) replaceURL(text string) string {
	results := regexp.MustCompile(`<(.*?)(\|.*?)?>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		text = strings.Replace(text, r[0], r[1], -1)
	}
	return text
}

func (u *User) userName(id string) string {
	// TODO dynamically update when new users are joining slack
	for _, us := range u.susers {
		if us.ID == id {
			if us.Profile.DisplayName != "" {
				return us.Profile.DisplayName
			}
			return us.Name
		}
	}
	if id == u.sinfo.User.ID {
		return u.sinfo.User.Name
	}
	return ""
}

func (u *User) isConnected() bool {
	return u.connected
}
