package importer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

// TrackTags represents audio metadata to be written to imported files.
type TrackTags struct {
	Title       string
	Artist      string
	Album       string
	AlbumArtist string
	TrackNumber int
	DiscNumber  int
	Year        int
	Date        string
	ISRC        string
}

// TagFLAC writes or replaces Vorbis comment metadata in a FLAC audio file.
func TagFLAC(filePath string, tags TrackTags) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read flac file %s: %w", filePath, err)
	}

	if len(data) < 4 || string(data[:4]) != "fLaC" {
		return fmt.Errorf("not a valid FLAC file: %s", filePath)
	}

	pos := 4
	var blocks [][]byte
	var blockTypes []byte

	for pos < len(data) {
		if pos+4 > len(data) {
			break
		}
		hdr := data[pos : pos+4]
		isLast := (hdr[0] & 0x80) != 0
		bType := hdr[0] & 0x7F
		bLen := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
		pos += 4

		if pos+bLen > len(data) {
			break
		}
		bData := data[pos : pos+bLen]
		pos += bLen

		// Drop existing Vorbis comment block (block type 4) so we write fresh canonical tags
		if bType != 4 {
			blocks = append(blocks, bData)
			blockTypes = append(blockTypes, bType)
		}

		if isLast {
			break
		}
	}

	audioData := data[pos:]

	// Construct Vorbis comment list
	var comments []string
	if tags.Title != "" {
		comments = append(comments, "TITLE="+tags.Title)
	}
	if tags.Artist != "" {
		comments = append(comments, "ARTIST="+tags.Artist)
	}
	if tags.Album != "" {
		comments = append(comments, "ALBUM="+tags.Album)
	}
	albumArtist := tags.AlbumArtist
	if albumArtist == "" {
		albumArtist = tags.Artist
	}
	if albumArtist != "" {
		comments = append(comments, "ALBUMARTIST="+albumArtist)
	}
	if tags.TrackNumber > 0 {
		comments = append(comments, fmt.Sprintf("TRACKNUMBER=%d", tags.TrackNumber))
	}
	if tags.DiscNumber > 0 {
		comments = append(comments, fmt.Sprintf("DISCNUMBER=%d", tags.DiscNumber))
	}
	if tags.Year > 0 {
		comments = append(comments, fmt.Sprintf("DATE=%d", tags.Year))
	} else if tags.Date != "" {
		comments = append(comments, "DATE="+tags.Date)
	}
	if tags.ISRC != "" {
		comments = append(comments, "ISRC="+tags.ISRC)
	}

	// Build Vorbis comment block payload
	var vc bytes.Buffer
	vendor := []byte("findr")
	_ = binary.Write(&vc, binary.LittleEndian, uint32(len(vendor)))
	vc.Write(vendor)
	_ = binary.Write(&vc, binary.LittleEndian, uint32(len(comments)))

	for _, c := range comments {
		cb := []byte(c)
		_ = binary.Write(&vc, binary.LittleEndian, uint32(len(cb)))
		vc.Write(cb)
	}

	vcBytes := vc.Bytes()

	// Place Vorbis comment block immediately after STREAMINFO (block 0)
	var newBlocks [][]byte
	var newTypes []byte

	if len(blocks) > 0 {
		newBlocks = append(newBlocks, blocks[0])
		newTypes = append(newTypes, blockTypes[0])
	}
	newBlocks = append(newBlocks, vcBytes)
	newTypes = append(newTypes, 4)
	if len(blocks) > 1 {
		newBlocks = append(newBlocks, blocks[1:]...)
		newTypes = append(newTypes, blockTypes[1:]...)
	}

	// Reassemble FLAC file bytes
	var out bytes.Buffer
	out.WriteString("fLaC")

	for i := range newBlocks {
		isLast := byte(0)
		if i == len(newBlocks)-1 {
			isLast = 0x80
		}
		bType := newTypes[i]
		bLen := len(newBlocks[i])

		hdr := []byte{
			isLast | bType,
			byte((bLen >> 16) & 0xFF),
			byte((bLen >> 8) & 0xFF),
			byte(bLen & 0xFF),
		}
		out.Write(hdr)
		out.Write(newBlocks[i])
	}

	out.Write(audioData)

	return os.WriteFile(filePath, out.Bytes(), 0644)
}
