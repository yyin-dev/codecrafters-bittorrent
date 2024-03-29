package main

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/codecrafters-io/bittorrent-starter-go/decode"
	"github.com/codecrafters-io/bittorrent-starter-go/encode"
)

const PipelineSize = 5

type Info struct {
	length      int
	name        string
	pieceLength int
	pieces      [](string) // binary format, not hex format
}

func (info *Info) hash() ([]byte, error) {
	dict := map[string](interface{}){
		"length":       info.length,
		"name":         info.name,
		"piece length": info.pieceLength,
		"pieces":       strings.Join(info.pieces, ""),
	}

	encoded_info, err := encode.Encode(dict)
	if err != nil {
		return []byte{}, err
	}

	h := sha1.New()
	h.Write([]byte(encoded_info))
	info_hash := h.Sum(nil)

	return info_hash, nil
}

type Torrent struct {
	trackerUrl string
	info       Info
}

func parseTorrent(s string) (*Torrent, error) {
	decoded_raw, err := decode.Decode(s)
	if err != nil {
		return nil, err
	}

	decoded := decoded_raw.(map[string](interface{}))
	trackerUrl := decoded["announce"].(string)
	info_dict := decoded["info"].(map[string](interface{}))
	length := info_dict["length"].(int)
	name := info_dict["name"].(string)
	pieceLength := info_dict["piece length"].(int)
	if err != nil {
		return nil, err
	}

	pieces_raw := info_dict["pieces"].(string)
	pieces := make([](string), 0)
	for i := 0; i < len(pieces_raw); i += 20 {
		pieceHash := (pieces_raw[i : i+20])
		pieces = append(pieces, pieceHash)
	}

	info := Info{
		length:      length,
		name:        name,
		pieceLength: pieceLength,
		pieces:      pieces,
	}

	torrent := Torrent{
		trackerUrl: trackerUrl,
		info:       info,
	}

	return &torrent, nil
}

func (torrent *Torrent) numPieces() int {
	return len(torrent.info.pieces)
}

func (torrent *Torrent) discoverPeers() ([]netip.AddrPort, error) {
	req, err := http.NewRequest("GET", torrent.trackerUrl, nil)
	if err != nil {
		return []netip.AddrPort{}, err
	}

	info_hash, err := torrent.info.hash()
	if err != nil {
		return []netip.AddrPort{}, err
	}

	query := req.URL.Query()
	query.Add("info_hash", string(info_hash))
	query.Add("peer_id", "deadbeefliveporkhaha")
	query.Add("port", "6881")
	query.Add("uploaded", "0")
	query.Add("downloaded", "0")
	query.Add("left", strconv.Itoa(torrent.info.length))
	query.Add("compact", "1")
	req.URL.RawQuery = query.Encode()

	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return []netip.AddrPort{}, err
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return []netip.AddrPort{}, err
	}

	decoded_resp, err := decode.Decode(string(body))
	if err != nil {
		return []netip.AddrPort{}, err
	}

	decoded_dict := decoded_resp.(map[string](interface{}))

	peer_addrports := make([]netip.AddrPort, 0)
	switch peers := decoded_dict["peers"].(type) {
	case string:
		// compact
		for i := 0; i < len(peers); i += 6 {
			// Each peer is represented with 6 bytes.
			// First 4 bytes is IP, where each byte is a number in the IP.
			// Last 2 bytes is port, in big-endian order.
			port := binary.BigEndian.Uint16([]byte(peers[i+4 : i+6]))
			addrBytes := [4]byte{peers[i], peers[i+1], peers[i+2], peers[i+3]}
			addrport := netip.AddrPortFrom(netip.AddrFrom4(addrBytes), port)
			peer_addrports = append(peer_addrports, addrport)
		}
	case [](interface{}):
		// noncompact
		for _, peerRaw := range peers {
			peer := peerRaw.(map[string]interface{})
			ipStr := peer["ip"].(string)
			addr, err := netip.ParseAddr(ipStr)
			if err != nil {
				return nil, err
			}
			port := peer["port"].(int)
			addrport := netip.AddrPortFrom(addr, uint16(port))
			peer_addrports = append(peer_addrports, addrport)
		}
	default:
		return nil, fmt.Errorf("unexpected case")
	}

	return peer_addrports, nil
}

// Assume handshake, bitfield, interested, unchoke are done already
func (torrent *Torrent) downloadPieceCore(piece int, conn net.Conn) ([]byte, error) {
	// for each block in the piece:
	// send a request message
	// read a piece message
	var pieceLength int
	if piece < torrent.numPieces()-1 {
		pieceLength = torrent.info.pieceLength
	} else {
		pieceLength = torrent.info.length % torrent.info.pieceLength
	}
	pieceData := make([]byte, pieceLength)
	DPrintf("pieceLength: %v\n", pieceLength)
	for blockIdx := 0; blockIdx*BlockMaxSize < pieceLength; blockIdx++ {
		// request message
		var blockSize int
		if (blockIdx+1)*BlockMaxSize < pieceLength {
			blockSize = BlockMaxSize
		} else {
			blockSize = pieceLength - blockIdx*BlockMaxSize
		}
		DPrintf("BlockIdx: %v, BlockSize: %v\n", blockIdx, blockSize)
		blockOffset := blockIdx * BlockMaxSize

		// 4-byte message length, 1-byte message id, and a payload of:
		// - 4-byte block index
		// - 4-byte block offset within the piece (in bytes)
		// - 4-byte block length
		// Note: message length starts counting from the message id.
		msgLengthBytes := uint32_to_bytes(13, 4)
		msgIdBytes := uint32_to_bytes(6, 1)
		pieceIdxBytes := uint32_to_bytes(uint32(piece), 4)
		blockOffsetBytes := uint32_to_bytes(uint32(blockOffset), 4)
		blockLengthBytes := uint32_to_bytes(uint32(blockSize), 4)
		bytesToWrite := []([]byte){msgLengthBytes, msgIdBytes, pieceIdxBytes, blockOffsetBytes, blockLengthBytes}

		var requestMsg [17]byte
		writeIdx := 0
		for _, bytes := range bytesToWrite {
			for _, b := range bytes {
				requestMsg[writeIdx] = b
				writeIdx++
			}
		}
		assert(writeIdx == len(requestMsg), "Expect all bytes written")
		bytesWritten, err := conn.Write(requestMsg[:])
		if err != nil {
			return []byte{}, err
		}
		assert(bytesWritten == len(requestMsg), "Expect to send the whole message")
		DPrintf("Sent out request msg")

		// piece message
		// 4-byter message length, 1-byte message id, and a payload of
		// - 4-byte block index
		// - 4-byte block offset within the piece (in bytes)
		// - data
		pieceMsgHdr := make([]byte, 5)
		bytesRead, err := conn.Read(pieceMsgHdr)
		if err != nil {
			return []byte{}, err
		}
		totalBytesToRead := int(binary.BigEndian.Uint32(pieceMsgHdr[0:4]))
		assert(bytesRead == 5, "Expect to read the full header")
		assert(pieceMsgHdr[4] == 7, "Expect message id = 7")
		totalBytesToRead -= 1

		pieceMsgMetadata := make([]byte, 8)
		bytesRead, err = conn.Read(pieceMsgMetadata)
		if err != nil {
			return []byte{}, err
		}
		assert(bytesRead == 8, "Expect to read 8 bytes of metadata")
		receivedPieceIdx := int(binary.BigEndian.Uint32(pieceMsgMetadata[:4]))
		receivedBlockStartOffset := int(binary.BigEndian.Uint32(pieceMsgMetadata[4:8]))
		assert(piece == receivedPieceIdx, "blockIdx doesn't match received")
		assert(blockOffset == receivedBlockStartOffset, "blockOffset doesn't match received")
		totalBytesToRead -= 8

		for writeOffset := 0; totalBytesToRead > 0; {
			bytesRead, err = conn.Read(pieceData[blockOffset+writeOffset:])
			if err != nil {
				return []byte{}, err
			}
			DPrintf("totalBytesToRead: %v, writeOffset+blockOffset: %v, bytesRead: %v, Finished at: %v\n",
				totalBytesToRead, writeOffset+blockOffset, bytesRead, writeOffset+blockOffset+bytesRead)

			totalBytesToRead -= bytesRead
			writeOffset += bytesRead
		}
	}

	return pieceData, nil
}

// Discover peers and exchange messages required before starting downloading pieces.
// Returns a connection to one of the peers. The caller should close the connection when finished.
func (torrent *Torrent) prepareForDownload() (net.Conn, error) {
	peers, err := torrent.discoverPeers()
	if err != nil {
		return nil, err
	}

	infoHash, err := torrent.info.hash()
	if err != nil {
		return nil, err
	}

	peer := peers[rand.Intn(len(peers))]
	DPrintf("Dialing peer %v...\n", peer)
	conn, err := net.Dial("tcp", peer.String())
	if err != nil {
		return nil, err
	}

	handshakeMsg := HandshakeMsg(infoHash, []byte("deadbeefliveporkhaha"))
	bytesWritten, err := conn.Write([]byte(handshakeMsg))
	assert(bytesWritten == 68, "Expect to write 68 bytes for handshake")
	if err != nil {
		return nil, err
	}

	// handshake response
	response := make([]byte, 68)
	bytesRead, err := conn.Read(response)
	if err != nil {
		return nil, err
	}
	assert(bytesRead == 68, "Expect handshake response to be 68 bytes")
	DPrintf("handshake response received\n")

	// bitfield message
	// Note: this message is optional in reality
	bitfieldMsgHdr := make([]byte, 5)
	bytesRead, err = conn.Read(bitfieldMsgHdr)
	if err != nil {
		return nil, err
	}
	assert(bytesRead == 5, "Expect to read 5 bytes")
	assert(uint8(bitfieldMsgHdr[4]) == 5, fmt.Sprintf("bitfield message should have message id = 5, but got %v", bitfieldMsgHdr[4]))
	numBytesBitfieldMsgPayload := int(binary.BigEndian.Uint32(bitfieldMsgHdr[:4])) - 1
	bitfieldMsgPayload := make([]byte, numBytesBitfieldMsgPayload)
	bytesRead, err = conn.Read(bitfieldMsgPayload)
	if err != nil {
		return nil, err
	}
	assert(bytesRead == numBytesBitfieldMsgPayload, "expect to read full bitfield payload")
	DPrintf("Bitfield received. Payload: %v\n", hex.EncodeToString(bitfieldMsgPayload))

	// send interested message
	interestedMsg := [5]byte{0, 0, 0, 1, 2}
	bytesWritten, err = conn.Write(interestedMsg[:])
	assert(bytesWritten == 5, "Expect to write 5 bytes for interested message")
	if err != nil {
		return nil, err
	}
	DPrintf("interested message sent\n")

	// unchoke message
	unchokeMsg := make([]byte, 5)
	bytesRead, err = conn.Read(unchokeMsg)
	if err != nil {
		return nil, err
	}
	assert(bytesRead == 5, fmt.Sprintf("Expect to read 5 bytes for unchoke message, but got %v bytes", bytesRead))
	assert(uint8(unchokeMsg[4]) == 1, fmt.Sprintf("unchoke message should have message id = 1, but got id = %v", uint8(unchokeMsg[4])))
	DPrintf("unchoke message received\n")

	return conn, nil
}

func (torrent *Torrent) downloadPiece(piece int) ([]byte, error) {
	conn, err := torrent.prepareForDownload()
	if err != nil {
		return []byte{}, err
	}
	defer conn.Close()

	pieceData, err := torrent.downloadPieceCore(piece, conn)
	if err != nil {
		return []byte{}, err
	}

	// check hash
	h := sha1.New()
	h.Write(pieceData)
	pieceHash := h.Sum(nil)
	expectedHash := torrent.info.pieces[piece]
	DPrintf("Hash of received piece: %v, expected piece hash: %v", pieceHash, []byte(expectedHash))
	assert(string(pieceHash) == string(expectedHash),
		fmt.Sprintf("Piece hash mismatch. Expected: %v (len=%v) (hex=%v), got: %v (len=%v) (hex=%v)\n",
			[]byte(expectedHash), len(expectedHash), hex.EncodeToString([]byte(expectedHash)),
			pieceHash, len(pieceHash), hex.EncodeToString([]byte(pieceHash))))

	return pieceData, nil
}

func (torrent *Torrent) downloadFile(outputFilename string) error {
	outputFile, err := os.OpenFile(outputFilename, os.O_CREATE|os.O_WRONLY, 0644)
	exit_on_error(err)
	defer outputFile.Close()

	conn, err := torrent.prepareForDownload()
	if err != nil {
		return err
	}
	defer conn.Close()

	for p := 0; p < torrent.numPieces(); p++ {
		piece, err := torrent.downloadPieceCore(p, conn)
		if err != nil {
			return err
		}

		_, err = outputFile.Write(piece)
		if err != nil {
			return err
		}
	}
	return nil
}

func (torrent *Torrent) requestMsgs() []([]byte) {
	msgs := make([]([]byte), 0)

	for piece := 0; piece < torrent.numPieces(); piece++ {
		var pieceLength int
		if piece < torrent.numPieces()-1 {
			pieceLength = torrent.info.pieceLength
		} else {
			pieceLength = torrent.info.length % torrent.info.pieceLength
		}
		for blockIdx := 0; blockIdx*BlockMaxSize < pieceLength; blockIdx++ {
			// request message
			var blockSize int
			if (blockIdx+1)*BlockMaxSize < pieceLength {
				blockSize = BlockMaxSize
			} else {
				blockSize = pieceLength - blockIdx*BlockMaxSize
			}
			DPrintf("piece: %v, BlockIdx: %v, BlockSize: %v\n", piece, blockIdx, blockSize)
			blockOffset := blockIdx * BlockMaxSize

			// 4-byte message length, 1-byte message id, and a payload of:
			// - 4-byte block index
			// - 4-byte block offset within the piece (in bytes)
			// - 4-byte block length
			// Note: message length starts counting from the message id.
			msgLengthBytes := uint32_to_bytes(13, 4)
			msgIdBytes := uint32_to_bytes(6, 1)
			pieceIdxBytes := uint32_to_bytes(uint32(piece), 4)
			blockOffsetBytes := uint32_to_bytes(uint32(blockOffset), 4)
			blockLengthBytes := uint32_to_bytes(uint32(blockSize), 4)
			bytesToWrite := []([]byte){msgLengthBytes, msgIdBytes, pieceIdxBytes, blockOffsetBytes, blockLengthBytes}

			var requestMsg [17]byte
			writeIdx := 0
			for _, bytes := range bytesToWrite {
				for _, b := range bytes {
					requestMsg[writeIdx] = b
					writeIdx++
				}
			}
			assert(writeIdx == len(requestMsg), "Expect all bytes written")
			msgs = append(msgs, requestMsg[:])
		}
	}

	return msgs
}

func (torrent *Torrent) downloadFilePipelining(outputFilename string) error {
	outputFile, err := os.OpenFile(outputFilename, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	conn, err := torrent.prepareForDownload()
	if err != nil {
		return err
	}

	allRequestMsgs := torrent.requestMsgs()

	// send out initial PipelineSize number of requests
	nextRequestMsg := 0
	for ; nextRequestMsg < PipelineSize; nextRequestMsg++ {
		numBytesWritten, err := conn.Write(allRequestMsgs[nextRequestMsg])
		if err != nil {
			return err
		}
		assert(numBytesWritten == len(allRequestMsgs[nextRequestMsg]), "expect full message to be sent")
	}

	for numBlocksToReceive := len(allRequestMsgs); numBlocksToReceive > 0; numBlocksToReceive-- {
		// piece message
		// 4-byter message length, 1-byte message id, and a payload of
		// - 4-byte block index
		// - 4-byte block offset within the piece (in bytes)
		// - data
		pieceMsgHdr := make([]byte, 5)
		bytesRead, err := conn.Read(pieceMsgHdr)
		if err != nil {
			return err
		}
		totalBytesToRead := int(binary.BigEndian.Uint32(pieceMsgHdr[0:4]))
		assert(bytesRead == 5, "Expect to read the full header")
		assert(pieceMsgHdr[4] == 7, fmt.Sprintf("Expect message id = 7, but get %v", pieceMsgHdr[4]))
		totalBytesToRead -= 1

		pieceMsgMetadata := make([]byte, 8)
		bytesRead, err = conn.Read(pieceMsgMetadata)
		if err != nil {
			return err
		}
		assert(bytesRead == 8, "Expect to read 8 bytes of metadata")
		pieceIdx := int(binary.BigEndian.Uint32(pieceMsgMetadata[:4]))
		blockStartInPiece := int(binary.BigEndian.Uint32(pieceMsgMetadata[4:8]))
		totalBytesToRead -= 8
		blockSize := totalBytesToRead

		blockData := make([]byte, blockSize)
		for writeOffset := 0; totalBytesToRead > 0; {
			bytesRead, err = conn.Read(blockData[writeOffset:])
			if err != nil {
				return err
			}
			DPrintf("totalBytesToRead: %v, writeOffset: %v, bytesRead: %v, Finished at: %v\n",
				totalBytesToRead, writeOffset, bytesRead, writeOffset+bytesRead)

			totalBytesToRead -= bytesRead
			writeOffset += bytesRead
		}

		// write blockData to correct position in file
		pieceStartOffsetGlobal := pieceIdx * BlockMaxSize
		blockStartOffsetGlobal := pieceStartOffsetGlobal + blockStartInPiece
		numBytesWritten, err := outputFile.WriteAt(blockData, int64(blockStartOffsetGlobal))
		if err != nil {
			return err
		}
		assert(numBytesWritten == blockSize, "Expect to write the full block")

		// send out next request message, if any left
		if nextRequestMsg < len(allRequestMsgs) {
			numBytesWritten, err := conn.Write(allRequestMsgs[nextRequestMsg])
			if err != nil {
				return err
			}
			assert(numBytesWritten == len(allRequestMsgs[nextRequestMsg]), "expect full message to be sent")

			nextRequestMsg++
			if nextRequestMsg == len(allRequestMsgs) {
				DPrintf("Finished sending all request messages")
			}
		}
	}

	// TODO: check hash

	return nil
}
