package wtserver

import (
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// handleDeleteSession processes a DeleteSession request for a client with given
// SessionID. The id is assumed to have been previously authenticated by the
// brontide connection.
func (s *Server) handleDeleteSession(peer Peer, id *wtdb.SessionID) error {
	var failCode wtwire.DeleteSessionCode

	// Delete all session data associated with id.
	err := s.cfg.DB.DeleteSession(*id)
	switch {
	case err == nil:
		failCode = wtwire.CodeOK

		log.Debugf("Session %s deleted", id)

	case err == wtdb.ErrSessionNotFound:
		failCode = wtwire.DeleteSessionCodeNotFound

	default:
		failCode = wtwire.CodeTemporaryFailure
	}

	return s.replyDeleteSession(peer, id, failCode)
}

// replyDeleteSession sends a DeleteSessionReply back to the peer containing the
// error code resulting from processes a DeleteSession request.
func (s *Server) replyDeleteSession(peer Peer, id *wtdb.SessionID,
	code wtwire.DeleteSessionCode) error {

	msg := &wtwire.DeleteSessionReply{
		Code: code,
	}

	err := s.sendMessage(peer, msg)
	if err != nil {
		log.Errorf("Unable to send DeleteSessionReply to %s", id)
	}

	// Return the write error if the request succeeded.
	if code == wtwire.CodeOK {
		return err
	}

	// Otherwise the request failed, return a connection failure to
	// disconnect the client.
	return &connFailure{
		ID:   *id,
		Code: code,
	}
}
