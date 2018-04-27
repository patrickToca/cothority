/*
Package darc in most of our projects we need some kind of access control to
protect resources. Instead of having a simple password or public key for
authentication, we want to have access control that can be: evolved with a
threshold number of keys be delegated. So instead of having a fixed list of
identities that are allowed to access a resource, the goal is to have an
evolving description of who is allowed or not to access a certain resource.

The primary type is a darc. Which contains a set of rules that what type of
permission are granted for any identity. A darc can be updated by performing an
evolution.  That is, the identities that have the "evolve" permission in the
old darc can creates a signature that signs off the new darc. Evolutions can be
performed any number of times, which creates a chain of darcs, also known as a
path. A path can be verified by starting at the oldest darc (also known as the
base darc), walking down the path and verifying the signature at every step.

As mentioned before, it is possible to perform delegation. For example, instead
of giving the "evolve" permission to (public key) identities, we can give it to
other darcs. For example, suppose the newest darc in some path, let's called it
darc_A, has the "evolve" permission set to true for another darc---darc_B, then
darc_B is allowed to evolve the path.

Of course, we do not want to have static rules that allows only a single
signer.  Our darc implementation supports an expression language where the user
can use logical operators to specify the rule.  For exmple, the expression
"darc:a & ed25519:b | ed25519:c" means that "darc:a" and at least one of
"ed25519:b" and "ed25519:c" must sign. For more information please see the
expression package.
*/
package darc

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/dedis/protobuf"
	"github.com/dedis/student_18_omniledger/omniledger/darc/expression"
	"gopkg.in/dedis/cothority.v2"
	"gopkg.in/dedis/kyber.v2"
	"gopkg.in/dedis/kyber.v2/sign/schnorr"
	"gopkg.in/dedis/kyber.v2/util/key"
)

const evolve = "_evolve"
const sign = "_sign"

// InitRules initialise a set of rules with the default actions "_evolve" and
// "_sign". Signers are joined with logical-Or, owners are also joined with
// logical-OR. If other expressions are needed, please set the rules manually.
func InitRules(owners []*Identity, signers []*Identity) Rules {
	rs := make(Rules)

	ownerIDs := make([]string, len(owners))
	for i, o := range owners {
		ownerIDs[i] = o.String()
	}
	rs[evolve] = expression.InitOrExpr(ownerIDs...)

	signerIDs := make([]string, len(signers))
	for i, s := range signers {
		signerIDs[i] = s.String()
	}
	rs[sign] = expression.InitOrExpr(signerIDs...)
	return rs
}

// NewDarc initialises a darc-structure given its owners and users. Note that
// the BaseID is empty if the Version is 0, it must be computed using
// GetBaseID.
func NewDarc(rules Rules, desc []byte) *Darc {
	return &Darc{
		Version:     0,
		Description: desc,
		Signatures:  []*Signature{},
		Rules:       rules,
	}
}

// Copy all the fields of a Darc except the signature
func (d *Darc) Copy() *Darc {
	dCopy := &Darc{
		Version:     d.Version,
		Description: copyBytes(d.Description),
		BaseID:      copyBytes(d.BaseID),
	}
	newRules := make(Rules)
	for k, v := range d.Rules {
		newRules[k] = v
	}
	dCopy.Rules = newRules
	return dCopy
}

// Equal returns true if both darcs point to the same data.
func (d *Darc) Equal(d2 *Darc) bool {
	return d.GetID().Equal(d2.GetID())
}

// ToProto returns a protobuf representation of the Darc-structure.
// We copy a darc first to keep only invariant fields which exclude
// the delegation signature.
func (d *Darc) ToProto() ([]byte, error) {
	dc := d.Copy()
	b, err := protobuf.Encode(dc)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// NewDarcFromProto interprets a protobuf-representation of the darc and
// returns a created Darc.
func NewDarcFromProto(protoDarc []byte) *Darc {
	d := &Darc{}
	protobuf.Decode(protoDarc, d)
	return d
}

// GetID returns the Darc ID, which is a digest of the values in the Darc.
// The digest does not include the signature or the path, only the version,
// description, base ID and the rules .
func (d Darc) GetID() ID {
	h := sha256.New()
	verBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(verBytes, d.Version)
	h.Write(verBytes)
	h.Write(d.Description)
	h.Write(d.BaseID)

	actions := make([]string, len(d.Rules))
	var i int
	for k := range d.Rules {
		actions[i] = string(k)
		i++
	}
	sort.Strings(actions)
	for _, a := range actions {
		h.Write([]byte(a))
		h.Write(d.Rules[Action(a)])
	}
	return h.Sum(nil)
}

// GetIdentityString returns the string representation of the ID.
func (d Darc) GetIdentityString() string {
	return NewIdentityDarc(d.GetID()).String()
}

// GetBaseID returns the base ID or the ID of this darc if its the
// first darc.
func (d Darc) GetBaseID() ID {
	if d.Version == 0 {
		return d.GetID()
	}
	return d.BaseID
}

// AddRule adds a new action expression-pair, the action must not exist.
func (r Rules) AddRule(a Action, expr expression.Expr) error {
	if _, ok := r[a]; ok {
		return errors.New("action already exists")
	}
	r[a] = expr
	return nil
}

// UpdateRule updates an existing action-expression pair, it cannot be the
// evolve or sign action.
func (r Rules) UpdateRule(a Action, expr expression.Expr) error {
	if isDefault(a) {
		return fmt.Errorf("cannot update action %s", a)
	}
	return r.updateRule(a, expr)
}

// DeleteRules deletes an action, it cannot delete the evolve or sign action.
func (r Rules) DeleteRules(a Action) error {
	if isDefault(a) {
		return fmt.Errorf("cannot delete action %s", a)
	}
	if _, ok := r[a]; !ok {
		return fmt.Errorf("action %s does not exist", a)
	}
	delete(r, a)
	return nil
}

// Contains checks if the action a is in the rules.
func (r Rules) Contains(a Action) bool {
	_, ok := r[a]
	return ok
}

// GetEvolutionExpr returns the expression that describes the evolution action.
func (r Rules) GetEvolutionExpr() expression.Expr {
	return r[evolve]
}

// GetSignExpr returns the expression that describes the sign action.
func (r Rules) GetSignExpr() expression.Expr {
	return r[sign]
}

// UpdateEvolution will update the "_evolve" action, which allows identities
// that satisfies the expression to evolve the Darc. Take extreme care when
// using this function.
func (r Rules) UpdateEvolution(expr expression.Expr) error {
	return r.updateRule(evolve, expr)
}

// UpdateSign will update the "_sign" action, which allows identities that
// satisfies the expression to sign on behalf of another darc.
func (r Rules) UpdateSign(expr expression.Expr) error {
	return r.updateRule(sign, expr)
}

func (r Rules) updateRule(a Action, expr expression.Expr) error {
	if _, ok := r[a]; !ok {
		return errors.New("action does not exist")
	}
	r[a] = expr
	return nil
}

func isDefault(action Action) bool {
	if action == evolve || action == sign {
		return true
	}
	return false
}

// Evolve evolves a darc, the latest valid darc needs to sign the new darc.
// Only if one of the previous owners signs off on the new darc will it be
// valid and accepted to sign on behalf of the old darc. The path can be nil
// unless if the previousOwner is an SignerEd25519 and found directly in the
// previous darc.
func (d *Darc) Evolve(path []*Darc, prevOwners ...*Signer) error {
	// We need to set Version and BaseID first because these will affect
	// the result of d.GetID().
	prevDarc := path[len(path)-1]
	d.Version = prevDarc.Version + 1
	if len(path) == 0 {
		return errors.New("path should not be empty")
	}
	d.BaseID = prevDarc.GetBaseID()

	tmpSigs := make([]*Signature, len(prevOwners))
	for i, prevOwner := range prevOwners {
		sig, err := NewDarcSignature(prevOwner, d.GetID(), path)
		if err != nil {
			return errors.New("error creating a darc signature for evolution: " + err.Error())
		}
		if sig == nil {
			return errors.New("the resulting signature is nil")
		}
		tmpSigs[i] = sig
	}
	d.Path = path
	d.Signatures = tmpSigs
	return nil
}

// IncrementVersion updates the version number of the Darc
func (d *Darc) IncrementVersion() {
	d.Version++
}

// Verify will check that the darc is correct, an error is returned if
// something is wrong.
func (d *Darc) Verify() error {
	return d.VerifyWithCB(func(s string) *Darc {
		return nil
	})
}

// VerifyWithCB will check that the darc is correct, an error is returned if
// something is wrong.  The caller should supply the callback getDarc because
// if one of the IDs in the expression is a Darc ID, then this function needs a
// way to retrieve the correct Darc according to that ID. Note that getDarc is
// responsible to return the newest Darc.
func (d *Darc) VerifyWithCB(getDarc func(string) *Darc) error {
	if d == nil {
		return errors.New("darc is nil")
	}
	if d.Version == 0 {
		return nil // nothing to verify on the genesis Darc
	}

	if len(d.Signatures) == 0 {
		return errors.New("no signatures")
	}

	if err := d.findPath(getDarc); err != nil {
		return err
	}

	var prev *Darc
	for i, curr := range d.Path {
		if prev == nil && curr.Version == 0 {
			prev = curr
			continue
		}
		if err := verifyOneEvolution(curr, prev, getDarc); err != nil {
			return fmt.Errorf("verification failed on index %d with error: %v", i, err)
		}
		prev = curr
	}

	signer := d.Path[len(d.Path)-1]
	return verifyOneEvolution(d, signer, getDarc)
}

// findPath will check if whether d.Path is unset, if it is, it'll try to
// populate it by looking up darcs using the getDarc callback.
func (d *Darc) findPath(getDarc func(string) *Darc) error {
	if len(d.Path) == 0 {
		baseID := NewIdentityDarc(d.GetBaseID())
		latest := getDarc(baseID.String())
		if latest == nil {
			return errors.New("couldn't find base darc")
		}
		var path []*Darc
		for _, p := range latest.Path {
			if d.Version > p.Version {
				path = append(path, p)
			} else {
				break
			}
		}
		pathDigest, err := hashAll(darcsMsg(path))
		if err != nil {
			return err
		}
		// Check that the path are equal to the digests in the
		// signatures.
		for _, sig := range d.Signatures {
			if !bytes.Equal(pathDigest, sig.PathDigest) {
				return fmt.Errorf("recomputed digest is not equal to the original")
			}
		}
		d.Path = path
	}
	if len(d.Path) == 0 {
		return errors.New("empty path")
	}
	return nil
}

// CheckRequest checks the given request and returns an error if it cannot be
// accepted.
func (d Darc) CheckRequest(r *Request) error {
	if !d.GetID().Equal(r.ID) {
		return fmt.Errorf("darc id mismatch")
	}
	if !d.Rules.Contains(r.Action) {
		return fmt.Errorf("%v does not exist", r.Action)
	}
	digest, err := r.Hash()
	if err != nil {
		return err
	}
	for i, id := range r.Identities {
		if err := id.Verify(digest, r.Signatures[i]); err != nil {
			return err
		}
	}
	validIDs := r.GetIdentityStrings()
	ok, err := expression.DefaultParser(d.Rules[r.Action], validIDs...)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("expression '%s' for the keys '%v' evaluated to false",
			d.Rules[r.Action], validIDs)
	}
	return nil
}

// String returns a human-readable string representation of the darc.
func (d Darc) String() string {
	s := fmt.Sprintf("ID:\t%x\nBase:\t%x\nVer:\t%d\nRules:", d.GetID(), d.GetBaseID(), d.Version)
	for k, v := range d.Rules {
		s += fmt.Sprintf("\n\t%s - \"%s\"", k, v)
	}
	for i, sig := range d.Signatures {
		if sig == nil {
			s += fmt.Sprintf("\n\t%d - <nil signature>", i)
		} else {
			s += fmt.Sprintf("\n\t%d - id: %s, sig: %x", i, sig.Signer.String(), sig.Signature)
		}
	}
	return s
}

// IsNull returns true if this DarcID is not initialised.
func (di ID) IsNull() bool {
	return di == nil
}

// Equal compares with another DarcID.
func (di ID) Equal(other ID) bool {
	return bytes.Equal([]byte(di), []byte(other))
}

// NewDarcSignature creates a new darc signature by hashing (baseID + pathMsg),
// where PathMsg is retrieved from a given signature path, and signing it
// with a given signer.
func NewDarcSignature(signer *Signer, id ID, path []*Darc) (*Signature, error) {
	if signer == nil {
		return nil, errors.New("signer missing")
	}
	if len(id) == 0 {
		return nil, errors.New("id missing")
	}
	if len(path) == 0 {
		return nil, errors.New("path missing")
	}

	// Create the message and then sign it, make sure the message is
	// re-created the same way for the verification.
	pathDigest, err := hashAll(darcsMsg(path))
	if err != nil {
		return nil, err
	}
	digest, err := hashAll(id, pathDigest)
	if err != nil {
		return nil, err
	}
	sig, err := signer.Sign(digest)
	if err != nil {
		return nil, err
	}
	s := Signature{Signature: sig, Signer: *signer.Identity(), PathDigest: pathDigest}
	return &s, nil
}

// pathContains checks whether the path contains the ID.
func (d *Darc) pathContains(cb func(*Darc) bool) bool {
	for _, p := range d.Path {
		if cb(p) {
			return true
		}
	}
	return false
}

// Verify returns nil if the signature is correct, or an error
// if something is wrong.
func (s *Signature) verify(path []*Darc, msg []byte, base ID) error {
	if base == nil {
		return errors.New("base-darc is missing")
	}
	if len(path) == 0 {
		return errors.New("no path stored in signaturepath")
	}
	sigBase := path[0].GetID()
	if !sigBase.Equal(base) {
		return errors.New("Base-darc is not at root of path")
	}
	digest, err := hashAll(msg, s.PathDigest)
	if err != nil {
		return err
	}
	return s.Signer.Verify(digest, s.Signature)
}

func hashAll(msgs ...[]byte) ([]byte, error) {
	h := sha256.New()
	for _, msg := range msgs {
		if _, err := h.Write(msg); err != nil {
			return nil, err
		}
	}
	return h.Sum(nil), nil
}

func darcsMsg(darcs []*Darc) []byte {
	if len(darcs) == 0 {
		return []byte{}
	}
	var path []byte
	for _, darc := range darcs {
		path = append(path, darc.GetID()...)
	}
	return path
}

// verifyOneEvolution verifies that one evolution is performed correctly. That
// is, there should exist a signature in the newDarc that is signed by one of
// the identities with the evolve permission in the oldDarc. The message is the
// signature path specified in the newDarc, its ID and the base ID of the darc.
func verifyOneEvolution(newDarc, prevDarc *Darc, getDarc func(string) *Darc) error {
	// check base ID
	if newDarc.BaseID == nil {
		return errors.New("nil base ID")
	}
	if !newDarc.GetBaseID().Equal(prevDarc.GetBaseID()) {
		return errors.New("base IDs are not equal")
	}

	// check version
	if newDarc.Version != prevDarc.Version+1 {
		return fmt.Errorf("incorrect version, new version should be %d but it is %d",
			prevDarc.Version+1, newDarc.Version)
	}

	// check that signers have the permission
	if err := evalExprWithSigs(
		prevDarc.Rules.GetEvolutionExpr(),
		getDarc,
		newDarc.Signatures...); err != nil {
		return err
	}

	// perform the verification
	for _, sig := range newDarc.Signatures {
		if err := sig.verify(newDarc.Path, newDarc.GetID(), prevDarc.GetBaseID()); err != nil {
			return err
		}
	}
	return nil
}

// evalExprWithSigs is a simple wrapper around
func evalExprWithSigs(expr expression.Expr, getDarc func(string) *Darc, sigs ...*Signature) error {
	signers := make([]string, len(sigs))
	for i, sig := range sigs {
		signers[i] = sig.Signer.String()
	}
	if err := evalExpr(expr, getDarc, signers...); err != nil {
		return err
	}
	return nil
}

// evalExpr checks whether the expression evaluates to true
// given a list of identities.
func evalExpr(expr expression.Expr, getDarc func(string) *Darc, ids ...string) error {
	Y := expression.InitParser(func(s string) bool {
		if strings.HasPrefix(s, "darc") {
			// getDarc is responsible for returning the latest Darc
			// but the path should contain the darc ID s.
			d := getDarc(s)
			if d.Verify() != nil {
				return false
			}
			// Evaluate the "sign" action only in the latest darc
			// because it may have revoked some rules in earlier
			// darcs. We do this recursively because there may be
			// further delegations.
			if !d.Rules.Contains(sign) {
				return false
			}
			// Recursively evaluate the sign expression until we
			// find the final signer with a ed25519 key.
			if err := evalExpr(d.Rules[sign], getDarc, ids...); err != nil {
				return false
			}
			return true
		}
		for _, id := range ids {
			if id == s {
				return true
			}
		}
		return false
	})
	res, err := expression.Evaluate(Y, expr)
	if err != nil {
		return fmt.Errorf("evaluation failed on '%s' with error: %v", expr, err)
	}
	if res != true {
		return fmt.Errorf("expression '%s' evaluated to false", expr)
	}
	return nil
}

// Type returns an integer representing the type of key held in the signer.
// It is compatible with Identity.Type. For an empty signer, -1 is returned.
func (s *Signer) Type() int {
	switch {
	case s.Ed25519 != nil:
		return 1
	case s.X509EC != nil:
		return 2
	default:
		return -1
	}
}

// Identity returns an identity struct with the pre initialised fields
// for the appropriate signer.
func (s *Signer) Identity() *Identity {
	switch s.Type() {
	case 1:
		return &Identity{Ed25519: &IdentityEd25519{Point: s.Ed25519.Point}}
	case 2:
		return &Identity{X509EC: &IdentityX509EC{Public: s.X509EC.Point}}
	default:
		return nil
	}
}

// Sign returns a signature in bytes for a given messages by the signer
func (s *Signer) Sign(msg []byte) ([]byte, error) {
	if msg == nil {
		return nil, errors.New("nothing to sign, message is empty")
	}
	switch s.Type() {
	case 0:
		return nil, errors.New("cannot sign with a darc")
	case 1:
		return s.Ed25519.Sign(msg)
	case 2:
		return s.X509EC.Sign(msg)
	default:
		return nil, errors.New("unknown signer type")
	}
}

// GetPrivate returns the private key, if one exists.
func (s *Signer) GetPrivate() (kyber.Scalar, error) {
	switch s.Type() {
	case 1:
		return s.Ed25519.Secret, nil
	case 0, 2:
		return nil, errors.New("signer lacks a private key")
	default:
		return nil, errors.New("signer is of unknown type")
	}
}

// Equal first checks the type of the two identities, and if they match,
// it returns if their data is the same.
func (id *Identity) Equal(id2 *Identity) bool {
	if id.Type() != id2.Type() {
		return false
	}
	switch id.Type() {
	case 0:
		return id.Darc.Equal(id2.Darc)
	case 1:
		return id.Ed25519.Equal(id2.Ed25519)
	case 2:
		return id.X509EC.Equal(id2.X509EC)
	}
	return false
}

// Type returns an int indicating what type of identity this is. If all
// identities are nil, it returns -1.
func (id *Identity) Type() int {
	switch {
	case id.Darc != nil:
		return 0
	case id.Ed25519 != nil:
		return 1
	case id.X509EC != nil:
		return 2
	}
	return -1
}

// TypeString returns the string of the type of the identity.
func (id *Identity) TypeString() string {
	switch id.Type() {
	case 0:
		return "darc"
	case 1:
		return "ed25519"
	case 2:
		return "x509ec"
	default:
		return "No identity"
	}
}

// String returns the string representation of the identity
func (id *Identity) String() string {
	switch id.Type() {
	case 0:
		return fmt.Sprintf("%s:%x", id.TypeString(), id.Darc.ID)
	case 1:
		return fmt.Sprintf("%s:%s", id.TypeString(), id.Ed25519.Point.String())
	case 2:
		return fmt.Sprintf("%s:%x", id.TypeString(), id.X509EC.Public)
	default:
		return "No identity"
	}
}

// Verify returns nil if the signature is correct, or an error if something
// went wrong.
func (id *Identity) Verify(msg, sig []byte) error {
	switch id.Type() {
	case 0:
		return errors.New("cannot verify a darc-signature")
	case 1:
		return id.Ed25519.Verify(msg, sig)
	case 2:
		return id.X509EC.Verify(msg, sig)
	default:
		return errors.New("unknown identity")
	}
}

// NewIdentityDarc creates a new darc identity struct given a darcid
func NewIdentityDarc(id ID) *Identity {
	return &Identity{
		Darc: &IdentityDarc{
			ID: id,
		},
	}
}

// Equal returns true if both IdentityDarcs point to the same data.
func (idd *IdentityDarc) Equal(idd2 *IdentityDarc) bool {
	return bytes.Compare(idd.ID, idd2.ID) == 0
}

// NewIdentityEd25519 creates a new Ed25519 identity struct given a point
func NewIdentityEd25519(point kyber.Point) *Identity {
	return &Identity{
		Ed25519: &IdentityEd25519{
			Point: point,
		},
	}
}

// Equal returns true if both IdentityEd25519 point to the same data.
func (ide *IdentityEd25519) Equal(ide2 *IdentityEd25519) bool {
	return ide.Point.Equal(ide2.Point)
}

// Verify returns nil if the signature is correct, or an error if something
// fails.
func (ide *IdentityEd25519) Verify(msg, sig []byte) error {
	return schnorr.Verify(cothority.Suite, ide.Point, msg, sig)
}

// NewIdentityX509EC creates a new X509EC identity struct given a point
func NewIdentityX509EC(public []byte) *Identity {
	return &Identity{
		X509EC: &IdentityX509EC{
			Public: public,
		},
	}
}

// Equal returns true if both IdentityX509EC point to the same data.
func (idkc *IdentityX509EC) Equal(idkc2 *IdentityX509EC) bool {
	return bytes.Compare(idkc.Public, idkc2.Public) == 0
}

type sigRS struct {
	R *big.Int
	S *big.Int
}

// Verify returns nil if the signature is correct, or an error if something
// fails.
func (idkc *IdentityX509EC) Verify(msg, s []byte) error {
	public, err := x509.ParsePKIXPublicKey(idkc.Public)
	if err != nil {
		return err
	}
	digest := sha512.Sum384(msg)
	sig := &sigRS{}
	_, err = asn1.Unmarshal(s, sig)
	if err != nil {
		return err
	}
	if ecdsa.Verify(public.(*ecdsa.PublicKey), digest[:], sig.R, sig.S) {
		return nil
	}
	return errors.New("Wrong signature")
}

// NewSignerEd25519 initializes a new SignerEd25519 given a public and private keys.
// If any of the given values is nil or both are nil, then a new key pair is generated.
// It returns a signer.
func NewSignerEd25519(point kyber.Point, secret kyber.Scalar) *Signer {
	if point == nil || secret == nil {
		kp := key.NewKeyPair(cothority.Suite)
		point, secret = kp.Public, kp.Private
	}
	return &Signer{Ed25519: &SignerEd25519{
		Point:  point,
		Secret: secret,
	}}
}

// Sign creates a schnorr signautre on the message
func (eds *SignerEd25519) Sign(msg []byte) ([]byte, error) {
	return schnorr.Sign(cothority.Suite, eds.Secret, msg)
}

// Hash computes the digest of the request, the identities and signatures are
// not included.
func (r *Request) Hash() ([]byte, error) {
	h := sha256.New()
	if _, err := h.Write(r.ID); err != nil {
		return nil, err
	}
	if _, err := h.Write([]byte(r.Action)); err != nil {
		return nil, err
	}
	if _, err := h.Write(r.Msg); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// GetIdentityStrings returns a slice of identity strings, this is useful for
// creating a parser.
func (r *Request) GetIdentityStrings() []string {
	res := make([]string, len(r.Identities))
	for i, id := range r.Identities {
		res[i] = id.String()
	}
	return res
}

// NewRequest creates a new request which can be verified by a Darc.
func NewRequest(darcID ID, action Action, msg []byte, signers ...*Signer) (*Request, error) {
	r := Request{
		ID:     darcID,
		Action: action,
		Msg:    msg,
	}

	digest, err := r.Hash()
	if err != nil {
		return nil, err
	}

	r.Signatures = make([][]byte, len(signers))
	r.Identities = make([]*Identity, len(signers))
	for i, signer := range signers {
		r.Identities[i] = signer.Identity()
		r.Signatures[i], err = signer.Sign(digest)
		if err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// NewSignerX509EC creates a new SignerX509EC - mostly for tests
func NewSignerX509EC() *Signer {
	return nil
}

// Sign creates a RSA signature on the message
func (kcs *SignerX509EC) Sign(msg []byte) ([]byte, error) {
	return nil, errors.New("not yet implemented")
}

func copyBytes(a []byte) []byte {
	b := make([]byte, len(a))
	copy(b, a)
	return b
}
