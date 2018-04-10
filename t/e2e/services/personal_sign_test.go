package rpc

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	acc "github.com/status-im/status-go/geth/account"
	"github.com/status-im/status-go/geth/params"
	"github.com/status-im/status-go/geth/signal"
	"github.com/status-im/status-go/services/personal"
	"github.com/status-im/status-go/sign"
	e2e "github.com/status-im/status-go/t/e2e"
	. "github.com/status-im/status-go/t/utils"
	"github.com/stretchr/testify/suite"
)

const (
	signDataString   = "0xBAADBEEF"
	accountNotExists = "0x00164ca341326a03b547c05B343b2E21eFAe2400"
)

type testParams struct {
	Title             string
	EnableUpstream    bool
	Account           string
	Password          string
	HandlerFactory    func(string, string) func(string)
	ExpectedError     error
	DontSelectAccount bool // to take advantage of the fact, that the default is `false`
}

func TestWeb3Calls(t *testing.T) {
	suite.Run(t, new(Web3TestSuite))
}

type Web3TestSuite struct {
	e2e.BackendTestSuite
}

func (s *Web3TestSuite) TestPersonalSign() {
	s.testPersonalSign(false)
}

func (s *Web3TestSuite) TestPersonalSignUpstream() {
	// Test upstream if that's not StatusChain
	if GetNetworkID() == params.StatusChainNetworkID {
		s.T().Skip()
	} else {
		s.testPersonalSign(true)
	}
}

func (s *Web3TestSuite) testPersonalSign(enableUpstream bool) {
	testCases := []testParams{
		{ // Success scenario
			Title:          "Success scenario",
			EnableUpstream: enableUpstream,
			Account:        TestConfig.Account1.Address,
			Password:       TestConfig.Account1.Password,
		},
		{
			Title:          "Error (transient), wrong password",
			EnableUpstream: enableUpstream,
			Account:        TestConfig.Account1.Address,
			Password:       TestConfig.Account1.Password,
			HandlerFactory: s.notificationHandlerWrongPassword,
		},
		{
			Title:          "Error, no such account",
			EnableUpstream: enableUpstream,
			Account:        accountNotExists,
			Password:       TestConfig.Account1.Password,
			ExpectedError:  personal.ErrInvalidPersonalSignAccount,
			HandlerFactory: s.notificationHandlerNoAccount,
		},
		{
			Title:          "Error, not the selected (but still known) account",
			EnableUpstream: enableUpstream,
			Account:        TestConfig.Account2.Address,
			Password:       TestConfig.Account2.Password,
			ExpectedError:  personal.ErrInvalidPersonalSignAccount,
			HandlerFactory: s.notificationHandlerInvalidAccount,
		},
		{
			Title:             "Error, no account is selected (transient)",
			EnableUpstream:    enableUpstream,
			Account:           TestConfig.Account1.Address,
			Password:          TestConfig.Account1.Password,
			HandlerFactory:    s.notificationHandlerNoAccountSelected,
			DontSelectAccount: true,
		},
	}

	for _, c := range testCases {
		s.runTest(c)
	}
}

// Utility methods
func (s *Web3TestSuite) notificationHandlerSuccess(account string, pass string) func(string) {
	return func(jsonEvent string) {
		s.notificationHandler(account, pass, nil)(jsonEvent)
	}
}

func (s *Web3TestSuite) notificationHandlerWrongPassword(account string, pass string) func(string) {
	return func(jsonEvent string) {
		s.notificationHandler(account, pass+"wrong", keystore.ErrDecrypt)(jsonEvent)
		s.notificationHandlerSuccess(account, pass)(jsonEvent)
	}
}

func (s *Web3TestSuite) notificationHandlerNoAccount(account string, pass string) func(string) {
	return func(jsonEvent string) {
		s.notificationHandler(account, pass, personal.ErrInvalidPersonalSignAccount)(jsonEvent)
	}
}

func (s *Web3TestSuite) notificationHandlerInvalidAccount(account string, pass string) func(string) {
	return func(jsonEvent string) {
		s.notificationHandler(account, pass, personal.ErrInvalidPersonalSignAccount)(jsonEvent)
	}
}

func (s *Web3TestSuite) notificationHandlerNoAccountSelected(account string, pass string) func(string) {
	return func(jsonEvent string) {
		s.notificationHandler(account, pass, acc.ErrNoAccountSelected)(jsonEvent)
		envelope := unmarshalEnvelope(jsonEvent)
		if envelope.Type == sign.EventSignRequestAdded {
			err := s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password)
			s.NoError(err)
		}
		s.notificationHandlerSuccess(account, pass)(jsonEvent)
	}
}

func (s *Web3TestSuite) initTest(upstreamEnabled bool) error {
	nodeConfig, err := MakeTestNodeConfig(GetNetworkID())
	s.NoError(err)

	nodeConfig.IPCEnabled = false
	nodeConfig.WSEnabled = false
	nodeConfig.HTTPHost = "" // to make sure that no HTTP interface is started

	if upstreamEnabled {
		networkURL, err := GetRemoteURL()
		s.NoError(err)

		nodeConfig.UpstreamConfig.Enabled = true
		nodeConfig.UpstreamConfig.URL = networkURL
	}

	return s.Backend.StartNode(nodeConfig)
}

func (s *Web3TestSuite) notificationHandler(account string, pass string, expectedError error) func(string) {
	return func(jsonEvent string) {
		envelope := unmarshalEnvelope(jsonEvent)
		if envelope.Type == sign.EventSignRequestAdded {
			event := envelope.Event.(map[string]interface{})
			id := event["id"].(string)
			s.T().Logf("Sign request added (will be completed shortly): {id: %s}\n", id)

			//check for the correct method name
			method := event["method"].(string)
			s.Equal(params.PersonalSignMethodName, method)
			//check the event data
			args := event["args"].(map[string]interface{})
			s.Equal(signDataString, args["data"].(string))
			s.Equal(account, args["account"].(string))

			e := s.Backend.ApproveSignRequest(id, pass).Error
			s.T().Logf("Sign request approved. {id: %s, acc: %s, err: %v}", id, account, e)
			if expectedError == nil {
				s.NoError(e, "cannot complete sign reauest[%v]: %v", id, e)
			} else {
				s.EqualError(e, expectedError.Error())
			}
		}
	}
}

func (s *Web3TestSuite) runTest(params testParams) {
	s.T().Logf("Test case '%s' begin", params.Title)
	defer s.T().Logf("Test case '%s' end", params.Title)

	if params.HandlerFactory == nil {
		params.HandlerFactory = s.notificationHandlerSuccess
	}

	err := s.initTest(params.EnableUpstream)
	s.NoError(err)
	defer func() {
		err := s.Backend.StopNode()
		s.NoError(err)
	}()

	signal.SetDefaultNodeNotificationHandler(params.HandlerFactory(params.Account, params.Password))

	if params.DontSelectAccount {
		s.NoError(s.Backend.Logout())
	} else {
		s.NoError(s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password))
	}

	basicCall := fmt.Sprintf(
		`{"jsonrpc":"2.0","method":"personal_sign","params":["%s", "%s"],"id":67}`,
		signDataString,
		params.Account)
	result := s.Backend.CallRPC(basicCall)
	if params.ExpectedError == nil {
		s.NotContains(result, "error")
	} else {
		s.Contains(result, params.ExpectedError.Error())
	}
}

func unmarshalEnvelope(jsonEvent string) signal.Envelope {
	var envelope signal.Envelope
	if e := json.Unmarshal([]byte(jsonEvent), &envelope); e != nil {
		panic(e)
	}
	return envelope
}
