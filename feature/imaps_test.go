package feature

import (
	"github.com/HouzuoGuo/laitos/email"
	"strings"
	"testing"
)

func TestIMAPS(t *testing.T) {
	if !TestIMAPAccounts.IsConfigured() {
		t.Skip()
	}
	if err := TestIMAPAccounts.Initialise(); err != nil {
		t.Fatal(err)
	}
	if err := TestIMAPAccounts.SelfTest(); err != nil {
		t.Fatal(err)
	}
	// IMAPS account test
	accountA := TestIMAPAccounts.Accounts["a"]
	if err := accountA.ConnectLoginSelect(); err != nil {
		t.Fatal(err)
	}
	if num, err := accountA.GetNumberMessages(); err != nil || num == 0 {
		t.Fatal(num, err)
	}
	if _, err := accountA.GetHeaders(1, 0); err == nil {
		t.Fatal("did not error")
	}
	if _, err := accountA.GetHeaders(2, 1); err == nil {
		t.Fatal("did not error")
	}
	// Retrieve headers, make sure it is retrieving three different emails
	headers, err := accountA.GetHeaders(1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 3 {
		t.Fatal(headers)
	}
	// Retrieve mail body
	msg, err := accountA.GetMessage(1)
	if err != nil {
		t.Fatal(err, msg)
	}
	err = email.WalkMessage([]byte(msg), func(prop email.BasicProperties, section []byte) (bool, error) {
		if prop.Subject == "" || prop.ContentType == "" || prop.FromAddress == "" || prop.ReplyAddress == "" {
			t.Fatalf("%+v", prop)
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	accountA.DisconnectLogout()
}

func TestIMAPAccounts_Initialise(t *testing.T) {
	accounts := IMAPAccounts{}
	if err := accounts.Initialise(); err != nil {
		t.Fatal(err)
	}
}

func TestIMAPAccounts_Execute(t *testing.T) {
	if !TestIMAPAccounts.IsConfigured() {
		t.Skip()
	}
	if err := TestIMAPAccounts.Initialise(); err != nil {
		t.Fatal(err)
	}
	if err := TestIMAPAccounts.SelfTest(); err != nil {
		t.Fatal(err)
	}
	// Nothing to do
	ret := TestIMAPAccounts.Execute(Command{TimeoutSec: 30, Content: "!@$!@%#%#$@%"})
	if ret.Error != ErrBadMailboxParam {
		t.Fatal(ret)
	}
	// Bad parameters
	if ret := TestIMAPAccounts.Execute(Command{TimeoutSec: 30, Content: MailboxList}); ret.Error != ErrBadMailboxParam {
		t.Fatal(ret)
	}
	if ret := TestIMAPAccounts.Execute(Command{TimeoutSec: 30, Content: MailboxList + "a 1, b"}); ret.Error != ErrBadMailboxParam {
		t.Fatal(ret)
	}
	if ret := TestIMAPAccounts.Execute(Command{TimeoutSec: 30, Content: MailboxRead}); ret.Error != ErrBadMailboxParam {
		t.Fatal(ret)
	}
	if ret := TestIMAPAccounts.Execute(Command{TimeoutSec: 30, Content: MailboxRead + "a b"}); ret.Error != ErrBadMailboxParam {
		t.Fatal(ret)
	}
	if ret := TestIMAPAccounts.Execute(Command{TimeoutSec: 30, Content: MailboxList + "does_not_exist 1, 2"}); strings.Index(ret.Error.Error(), "find box") == -1 {
		t.Fatal(ret)
	}
	if ret := TestIMAPAccounts.Execute(Command{TimeoutSec: 30, Content: MailboxRead + "does_not_exist 1"}); strings.Index(ret.Error.Error(), "find box") == -1 {
		t.Fatal(ret)
	}
	if ret := TestIMAPAccounts.Execute(Command{TimeoutSec: 30, Content: MailboxList + "a 100000000, 100"}); strings.Index(ret.Error.Error(), "skip+count") == -1 {
		t.Fatal(ret)
	}
	// List latest messages
	ret = TestIMAPAccounts.Execute(Command{TimeoutSec: 30, Content: MailboxList + "a 10, 5"})
	t.Log("List", ret.Output)
	if ret.Error != nil || len(ret.Output) < 50 || len(ret.Output) > 1000 {
		t.Fatal(ret)
	}
	// Read one message
	ret2 := TestIMAPAccounts.Execute(Command{TimeoutSec: 30, Content: MailboxRead + "a 2"})
	t.Log("Read", ret2.Output)
	if ret2.Error != nil || len(ret2.Output) < 1 {
		t.Fatal(ret)
	}
}
