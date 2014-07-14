/*
 Copyright 2013-2014 Canonical Ltd.

 This program is free software: you can redistribute it and/or modify it
 under the terms of the GNU General Public License version 3, as published
 by the Free Software Foundation.

 This program is distributed in the hope that it will be useful, but
 WITHOUT ANY WARRANTY; without even the implied warranties of
 MERCHANTABILITY, SATISFACTORY QUALITY, or FITNESS FOR A PARTICULAR
 PURPOSE.  See the GNU General Public License for more details.

 You should have received a copy of the GNU General Public License along
 with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package service

import (
	"encoding/json"
	"sync"

	"code.google.com/p/go-uuid/uuid"

	"launchpad.net/ubuntu-push/bus"
	"launchpad.net/ubuntu-push/bus/emblemcounter"
	"launchpad.net/ubuntu-push/bus/haptic"
	"launchpad.net/ubuntu-push/bus/notifications"
	"launchpad.net/ubuntu-push/click"
	"launchpad.net/ubuntu-push/launch_helper"
	"launchpad.net/ubuntu-push/logger"
	"launchpad.net/ubuntu-push/messaging"
	"launchpad.net/ubuntu-push/nih"
	"launchpad.net/ubuntu-push/sounds"
	"launchpad.net/ubuntu-push/util"
)

type messageHandler func(*click.AppId, string, *launch_helper.HelperOutput) error

// PostalService is the dbus api
type PostalService struct {
	DBusService
	mbox              map[string][]string
	msgHandler        messageHandler
	HelperLauncher    launch_helper.HelperLauncher
	messagingMenu     *messaging.MessagingMenu
	emblemcounterEndp bus.Endpoint
	hapticEndp        bus.Endpoint
	notificationsEndp bus.Endpoint
	actionsCh         <-chan *notifications.RawAction
}

var (
	PostalServiceBusAddress = bus.Address{
		Interface: "com.ubuntu.Postal",
		Path:      "/com/ubuntu/Postal",
		Name:      "com.ubuntu.Postal",
	}
)

var (
	SystemUpdateUrl = "settings:///system/system-update"
)

// NewPostalService() builds a new service and returns it.
func NewPostalService(busEndp bus.Endpoint, notificationsEndp bus.Endpoint, emblemcounterEndp bus.Endpoint, hapticEndp bus.Endpoint, installedChecker click.InstalledChecker, log logger.Logger) *PostalService {
	var svc = &PostalService{}
	svc.Log = log
	svc.Bus = busEndp
	svc.installedChecker = installedChecker
	svc.messagingMenu = messaging.New(log)
	svc.HelperLauncher = launch_helper.NewTrivialHelperLauncher(log)
	svc.notificationsEndp = notificationsEndp
	svc.emblemcounterEndp = emblemcounterEndp
	svc.hapticEndp = hapticEndp
	svc.msgHandler = svc.messageHandler
	return svc
}

// SetMessageHandler() sets the message-handling callback
func (svc *PostalService) SetMessageHandler(callback messageHandler) {
	svc.lock.RLock()
	defer svc.lock.RUnlock()
	svc.msgHandler = callback
}

// GetMessageHandler() returns the (possibly nil) messaging handler callback
func (svc *PostalService) GetMessageHandler() messageHandler {
	svc.lock.RLock()
	defer svc.lock.RUnlock()
	return svc.msgHandler
}

// Start() dials the bus, grab the name, and listens for method calls.
func (svc *PostalService) Start() error {
	if err := svc.takeTheBus(); err != nil {
		return err
	}
	return svc.DBusService.Start(bus.DispatchMap{
		"PopAll": svc.notifications,
		"Post":   svc.inject,
	}, PostalServiceBusAddress)
}

func (svc *PostalService) takeTheBus() error {
	var wg sync.WaitGroup
	endps := []bus.Endpoint{
		svc.notificationsEndp,
		svc.emblemcounterEndp,
		svc.hapticEndp,
	}
	wg.Add(len(endps))
	for _, endp := range endps {
		go func(endp bus.Endpoint) {
			util.NewAutoRedialer(endp).Redial()
			wg.Done()
		}(endp)
	}
	wg.Wait()
	actionsCh, err := notifications.Raw(svc.notificationsEndp, svc.Log).WatchActions()
	if err == nil {
		svc.actionsCh = actionsCh
	}

	return err
}

func (svc *PostalService) notifications(path string, args, _ []interface{}) ([]interface{}, error) {
	app, err := svc.grabDBusPackageAndAppId(path, args, 0)
	if err != nil {
		return nil, err
	}

	svc.lock.Lock()
	defer svc.lock.Unlock()

	if svc.mbox == nil {
		return []interface{}{[]string(nil)}, nil
	}
	msgs := svc.mbox[app.Original()]
	delete(svc.mbox, app.Original())

	return []interface{}{msgs}, nil
}

var newNid = uuid.New

func (svc *PostalService) inject(path string, args, _ []interface{}) ([]interface{}, error) {
	app, err := svc.grabDBusPackageAndAppId(path, args, 1)
	if err != nil {
		return nil, err
	}
	notif, ok := args[1].(string)
	if !ok {
		return nil, ErrBadArgType
	}

	nid := newNid()

	return nil, svc.Inject(app, nid, notif)
}

// Inject() signals to an application over dbus that a notification
// has arrived.
func (svc *PostalService) Inject(app *click.AppId, nid string, notif string) error {
	svc.lock.Lock()
	defer svc.lock.Unlock()
	if svc.mbox == nil {
		svc.mbox = make(map[string][]string)
	}
	output := svc.HelperLauncher.Run(app, []byte(notif))
	appId := app.Original()
	// XXX also track the nid in the mbox
	svc.mbox[appId] = append(svc.mbox[appId], string(output.Message))

	if svc.msgHandler != nil {
		err := svc.msgHandler(app, nid, output)
		if err != nil {
			svc.DBusService.Log.Errorf("msgHandler returned %v", err)
			return err
		}
		svc.DBusService.Log.Debugf("call to msgHandler successful")
	}

	return svc.Bus.Signal("Post", "/"+string(nih.Quote([]byte(app.Package))), []interface{}{appId})
}

func (svc *PostalService) messageHandler(app *click.AppId, nid string, output *launch_helper.HelperOutput) error {
	svc.messagingMenu.Present(app, nid, output.Notification)
	nots := notifications.Raw(svc.notificationsEndp, svc.Log)
	_, err := nots.Present(app, nid, output.Notification)
	emblemcounter.New(svc.emblemcounterEndp, svc.Log).Present(app, nid, output.Notification)
	haptic.New(svc.hapticEndp, svc.Log).Present(app, nid, output.Notification)
	sounds.New(svc.Log).Present(app, nid, output.Notification)
	return err
}

func (svc *PostalService) InjectBroadcast() (uint32, error) {
	icon := "update_manager_icon"
	summary := "There's an updated system image."
	body := "Tap to open the system updater."
	actions := []string{"Switch to app"} // action value not visible on the phone
	card := &launch_helper.Card{Icon: icon, Summary: summary, Body: body, Actions: actions, Popup: true}
	helperOutput := &launch_helper.HelperOutput{[]byte(""), &launch_helper.Notification{Card: card}}
	jsonNotif, err := json.Marshal(helperOutput)
	if err != nil {
		// XXX: how can we test this branch?
		svc.Log.Errorf("Failed to marshal notification: %v - %v", helperOutput, err)
		return 0, err
	}
	appId, _ := click.ParseAppId("_ubuntu-push-client")
	return 0, svc.Inject(appId, SystemUpdateUrl, string(jsonNotif))
}
