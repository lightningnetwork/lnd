package wtserver

import (
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// handleCreateSession processes a CreateSession message from the peer, and returns
// a CreateSessionReply in response. This method will only succeed if no existing
// session info is known about the session id. If an existing session is found,
// the reward address is returned in case the client lost our reply.
func (s *Server) handleCreateSession(peer Peer, id *wtdb.SessionID,
	req *wtwire.CreateSession) error {

	// TODO(conner): validate accept against policy

	// Query the db for session info belonging to the client's session id.
	existingInfo, err := s.cfg.DB.GetSessionInfo(id)
	switch {

	// We already have a session, though it is currently unused. We'll allow
	// the client to recommit the session if it wanted to change the policy.
	case err == nil && existingInfo.LastApplied == 0:

	// We already have a session corresponding to this session id, return an
	// error signaling that it already exists in our database. We return the
	// reward address to the client in case they were not able to process
	// our reply earlier.
	case err == nil && existingInfo.LastApplied > 0:
		log.Debugf("Already have session for %s", id)
		return s.replyCreateSession(
			peer, id, wtwire.CreateSessionCodeAlreadyExists,
			existingInfo.LastApplied, existingInfo.RewardAddress,
		)

	// Some other database error occurred, return a temporary failure.
	case err != wtdb.ErrSessionNotFound:
		log.Errorf("unable to load session info for %s", id)
		return s.replyCreateSession(
			peer, id, wtwire.CodeTemporaryFailure, 0, nil,
		)
	}

	// Ensure that the requested blob type is supported by our tower.
	if !blob.IsSupportedType(req.BlobType) {
		log.Debugf("Rejecting CreateSession from %s, unsupported blob "+
			"type %s", id, req.BlobType)
		return s.replyCreateSession(
			peer, id, wtwire.CreateSessionCodeRejectBlobType, 0,
			nil,
		)
	}

	// If the request asks for a reward session and the tower has them
	// disabled, we will reject the request.
	if s.cfg.DisableReward && req.BlobType.Has(blob.FlagReward) {
		log.Debugf("Rejecting CreateSession from %s, reward "+
			"sessions disabled", id)
		return s.replyCreateSession(
			peer, id, wtwire.CreateSessionCodeRejectBlobType, 0,
			nil,
		)
	}

	// Now that we've established that this session does not exist in the
	// database, retrieve the sweep address that will be given to the
	// client. This address is to be included by the client when signing
	// sweep transactions destined for this tower, if its negotiated output
	// is not dust.
	var rewardScript []byte
	if req.BlobType.Has(blob.FlagReward) {
		rewardAddress, err := s.cfg.NewAddress()
		if err != nil {
			log.Errorf("Unable to generate reward addr for %s: %v",
				id, err)
			return s.replyCreateSession(
				peer, id, wtwire.CodeTemporaryFailure, 0, nil,
			)
		}

		// Construct the pkscript the client should pay to when signing
		// justice transactions for this session.
		rewardScript, err = txscript.PayToAddrScript(rewardAddress)
		if err != nil {
			log.Errorf("Unable to generate reward script for "+
				"%s: %v", id, err)
			return s.replyCreateSession(
				peer, id, wtwire.CodeTemporaryFailure, 0, nil,
			)
		}
	}

	// TODO(conner): create invoice for upfront payment

	// Assemble the session info using the agreed upon parameters, reward
	// address, and session id.
	info := wtdb.SessionInfo{
		ID: *id,
		Policy: wtpolicy.Policy{
			TxPolicy: wtpolicy.TxPolicy{
				BlobType:     req.BlobType,
				RewardBase:   req.RewardBase,
				RewardRate:   req.RewardRate,
				SweepFeeRate: req.SweepFeeRate,
			},
			MaxUpdates: req.MaxUpdates,
		},
		RewardAddress: rewardScript,
	}

	// Insert the session info into the watchtower's database. If
	// successful, the session will now be ready for use.
	err = s.cfg.DB.InsertSessionInfo(&info)
	if err != nil {
		log.Errorf("Unable to create session for %s: %v", id, err)
		return s.replyCreateSession(
			peer, id, wtwire.CodeTemporaryFailure, 0, nil,
		)
	}

	log.Infof("Accepted session for %s", id)

	return s.replyCreateSession(
		peer, id, wtwire.CodeOK, 0, rewardScript,
	)
}

// replyCreateSession sends a response to a CreateSession from a client. If the
// status code in the reply is OK, the error from the write will be bubbled up.
// Otherwise, this method returns a connection error to ensure we don't continue
// communication with the client.
func (s *Server) replyCreateSession(peer Peer, id *wtdb.SessionID,
	code wtwire.ErrorCode, lastApplied uint16, data []byte) error {

	if s.cfg.NoAckCreateSession {
		return &connFailure{
			ID:   *id,
			Code: code,
		}
	}

	msg := &wtwire.CreateSessionReply{
		Code:        code,
		LastApplied: lastApplied,
		Data:        data,
	}

	err := s.sendMessage(peer, msg)
	if err != nil {
		log.Errorf("unable to send CreateSessionReply to %s", id)
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
