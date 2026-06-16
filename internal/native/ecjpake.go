package native

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

var curve = secp256k1.S256()

type ECJPAKE struct {
	role   string
	secret *big.Int

	x1Priv, x1PubX, x1PubY *big.Int
	x2Priv, x2PubX, x2PubY *big.Int
	X2PubX, X2PubY         *big.Int
	X3PubX, X3PubY         *big.Int

	failed          bool
	wroteR1, readR1 bool
	wroteR2, readR2 bool
}

func NewECJPAKE(role, passcode string) (*ECJPAKE, error) {
	if role != "client" && role != "server" {
		return nil, fmt.Errorf("role must be 'client' or 'server'")
	}
	return &ECJPAKE{role: role, secret: new(big.Int).SetBytes([]byte(passcode))}, nil
}

func (e *ECJPAKE) WriteRoundOne() ([]byte, error) {
	if e.failed {
		return nil, fmt.Errorf("reusing failed ECJPAKE context is insecure")
	}
	if e.wroteR1 || e.wroteR2 || e.readR2 {
		return nil, fmt.Errorf("wrong step")
	}
	e.wroteR1 = true

	Gx, Gy := curve.Gx, curve.Gy
	e.x1Priv, e.x1PubX, e.x1PubY = genKeyPair()
	zkp1 := schnorrZKP(Gx, Gy, e.x1PubX, e.x1PubY, e.x1Priv, e.role)
	e.x2Priv, e.x2PubX, e.x2PubY = genKeyPair()
	zkp2 := schnorrZKP(Gx, Gy, e.x2PubX, e.x2PubY, e.x2Priv, e.role)

	result := make([]byte, 0, 330)
	result = append(result, encodePoint66(e.x1PubX, e.x1PubY)...)
	result = append(result, zkp1...)
	result = append(result, encodePoint66(e.x2PubX, e.x2PubY)...)
	result = append(result, zkp2...)
	return result, nil
}

func (e *ECJPAKE) ReadRoundOne(data []byte) error {
	if e.failed {
		return fmt.Errorf("reusing failed ECJPAKE context is insecure")
	}
	if e.readR1 || e.wroteR2 || e.readR2 {
		return fmt.Errorf("wrong step")
	}
	e.readR1 = true

	otherRole := other(e.role)
	Gx, Gy := curve.Gx, curve.Gy
	off := 0

	x2X, x2Y, err := decodePoint66(data[off : off+66])
	if err != nil {
		e.failed = true
		return err
	}
	off += 66
	V1x, V1y, r1 := parseZKP(data[off : off+99])
	off += 99
	if !verifyZKP(Gx, Gy, x2X, x2Y, V1x, V1y, r1, otherRole) {
		e.failed = true
		return fmt.Errorf("round one ZKP1 failed")
	}

	x3X, x3Y, err := decodePoint66(data[off : off+66])
	if err != nil {
		e.failed = true
		return err
	}
	off += 66
	V2x, V2y, r2 := parseZKP(data[off : off+99])
	if !verifyZKP(Gx, Gy, x3X, x3Y, V2x, V2y, r2, otherRole) {
		e.failed = true
		return fmt.Errorf("round one ZKP2 failed")
	}

	e.X2PubX, e.X2PubY = x2X, x2Y
	e.X3PubX, e.X3PubY = x3X, x3Y
	return nil
}

func (e *ECJPAKE) WriteRoundTwo() ([]byte, error) {
	if e.failed {
		return nil, fmt.Errorf("reusing failed ECJPAKE context is insecure")
	}
	if e.wroteR2 || !e.wroteR1 || !e.readR1 {
		return nil, fmt.Errorf("wrong step")
	}
	e.wroteR2 = true

	gx, gy := ecAdd(e.x1PubX, e.x1PubY, e.X2PubX, e.X2PubY)
	gx, gy = ecAdd(gx, gy, e.X3PubX, e.X3PubY)

	n := randomNPlusSecret(e.secret)
	x := new(big.Int).Mul(e.x2Priv, n)
	x.Mod(x, curve.N)

	Ax, Ay := ecMul(gx, gy, x)
	zkp := schnorrZKP(gx, gy, Ax, Ay, x, e.role)

	var result []byte
	if e.role == "server" {
		result = make([]byte, 0, 168)
		result = append(result, 0x03)
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], 22)
		result = append(result, hdr[:]...)
	} else {
		result = make([]byte, 0, 165)
	}
	result = append(result, encodePoint66(Ax, Ay)...)
	result = append(result, zkp...)
	return result, nil
}

// ReadRoundTwo 解析对方 Round 2 并计算共享密钥
// gateway.js: v = o.add(this.#_.mul(m).neg()).mul(this.#T.getPrivate())
// 即 v = (A + X3 * (-m)) * x2_priv = (A - X3 * m) * x2_priv
func (e *ECJPAKE) ReadRoundTwo(data []byte) ([]byte, error) {
	if e.failed {
		return nil, fmt.Errorf("reusing failed ECJPAKE context is insecure")
	}
	if e.readR2 || !e.wroteR1 || !e.readR1 {
		return nil, fmt.Errorf("wrong step")
	}
	e.readR2 = true

	otherRole := other(e.role)
	off := 0
	if e.role == "client" {
		off += 3
	}

	Ax, Ay, err := decodePoint66(data[off : off+66])
	if err != nil {
		e.failed = true
		return nil, err
	}
	off += 66

	Vx, Vy, r := parseZKP(data[off : off+99])

	// g = x1_pub + x2_pub + X2_pub
	gx, gy := ecAdd(e.x1PubX, e.x1PubY, e.x2PubX, e.x2PubY)
	gx, gy = ecAdd(gx, gy, e.X2PubX, e.X2PubY)
	if !verifyZKP(gx, gy, Ax, Ay, Vx, Vy, r, otherRole) {
		e.failed = true
		return nil, fmt.Errorf("round two ZKP failed")
	}

	// gateway.js:
	//   y = random * curve_order + secret
	//   m = x2_priv * y
	//   v = (A - X3 * m) * x2_priv
	//
	// 实现：v = (A + X3 * (-m)) * x2_priv
	y := randomNPlusSecret(e.secret)
	m := new(big.Int).Mul(e.x2Priv, y)
	m.Mod(m, curve.N)

	// -m mod N
	negM := new(big.Int).Neg(m)
	negM.Mod(negM, curve.N)

	// X3 * (-m)
	X3negMx, X3negMy := ecMul(e.X3PubX, e.X3PubY, negM)

	// A + X3 * (-m)
	sumX, sumY := ecAdd(Ax, Ay, X3negMx, X3negMy)

	// (A + X3 * (-m)) * x2_priv
	vx, _ := ecMul(sumX, sumY, e.x2Priv)

	vxBytes := padTo32(vx.Bytes())
	hash := sha256.Sum256(vxBytes)
	return hash[:], nil
}

// === 辅助函数 ===

func genKeyPair() (privX, pubX, pubY *big.Int) { //nolint:all
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		panic("secp256k1 keygen failed: " + err.Error())
	}
	pub := priv.PubKey()
	b := priv.Key.Bytes()
	return new(big.Int).SetBytes(b[:]), pub.X(), pub.Y()
}

func ecAdd(x1, y1, x2, y2 *big.Int) (*big.Int, *big.Int) {
	return curve.Add(x1, y1, x2, y2)
}

func ecMul(x, y *big.Int, k *big.Int) (*big.Int, *big.Int) {
	k2 := new(big.Int).Mod(k, curve.N)
	return curve.ScalarMult(x, y, k2.Bytes())
}

func randomNPlusSecret(secret *big.Int) *big.Int {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	r := new(big.Int).SetBytes(b)
	n := new(big.Int).Mul(r, curve.N)
	n.Add(n, secret)
	return n
}

func encodePoint66(x, y *big.Int) []byte {
	buf := make([]byte, 66)
	buf[0] = 0x41
	buf[1] = 0x04
	copy(buf[2:34], padTo32(x.Bytes()))
	copy(buf[34:66], padTo32(y.Bytes()))
	return buf
}

func encodePoint69(x, y *big.Int) []byte {
	buf := make([]byte, 69)
	binary.BigEndian.PutUint32(buf[0:4], 65)
	buf[4] = 0x04
	copy(buf[5:37], padTo32(x.Bytes()))
	copy(buf[37:69], padTo32(y.Bytes()))
	return buf
}

func decodePoint66(data []byte) (*big.Int, *big.Int, error) {
	if len(data) < 66 {
		return nil, nil, fmt.Errorf("point too short: %d", len(data))
	}
	x := new(big.Int).SetBytes(data[2:34])
	y := new(big.Int).SetBytes(data[34:66])
	if !curve.IsOnCurve(x, y) {
		return nil, nil, fmt.Errorf("point not on curve")
	}
	return x, y, nil
}

func padTo32(b []byte) []byte {
	if len(b) >= 32 {
		if len(b) > 32 {
			return b[len(b)-32:]
		}
		return b
	}
	p := make([]byte, 32)
	copy(p[32-len(b):], b)
	return p
}

func schnorrZKP(gx, gy, pubX, pubY, priv *big.Int, role string) []byte {
	vBytes := make([]byte, 32)
	if _, err := rand.Read(vBytes); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	v := new(big.Int).SetBytes(vBytes)
	v.Mod(v, curve.N)
	if v.Sign() == 0 {
		v.SetInt64(1)
	}

	Vx, Vy := ecMul(gx, gy, v)
	c := zkpChallenge(gx, gy, Vx, Vy, pubX, pubY, role)

	r := new(big.Int).Mul(c, priv)
	r.Sub(v, r)
	r.Mod(r, curve.N)

	proof := make([]byte, 0, 99)
	proof = append(proof, encodePoint66(Vx, Vy)...)
	proof = append(proof, 0x20)
	proof = append(proof, padTo32(r.Bytes())...)
	return proof
}

func zkpChallenge(gx, gy, Vx, Vy, pubX, pubY *big.Int, role string) *big.Int {
	gb := encodePoint69(gx, gy)
	vb := encodePoint69(Vx, Vy)
	pb := encodePoint69(pubX, pubY)
	rb := []byte(role)
	rl := make([]byte, 4)
	binary.BigEndian.PutUint32(rl, uint32(len(rb)))

	buf := make([]byte, 0, len(gb)+len(vb)+len(pb)+4+len(rb))
	buf = append(buf, gb...)
	buf = append(buf, vb...)
	buf = append(buf, pb...)
	buf = append(buf, rl...)
	buf = append(buf, rb...)

	h := sha256.Sum256(buf)
	return new(big.Int).SetBytes(h[:])
}

func verifyZKP(gx, gy, pubX, pubY, expVx, expVy *big.Int, r *big.Int, role string) bool {
	c := zkpChallenge(gx, gy, expVx, expVy, pubX, pubY, role)
	pcX, pcY := ecMul(pubX, pubY, c)
	grX, grY := ecMul(gx, gy, r)
	ax, ay := ecAdd(pcX, pcY, grX, grY)
	return ax.Cmp(expVx) == 0 && ay.Cmp(expVy) == 0
}

func parseZKP(data []byte) (vx, vy, r *big.Int) {
	vx, vy, _ = decodePoint66(data[0:66])
	r = new(big.Int).SetBytes(data[67:99])
	return
}

func other(role string) string {
	if role == "client" {
		return "server"
	}
	return "client"
}
