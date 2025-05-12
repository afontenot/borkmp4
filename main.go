package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"github.com/abema/go-mp4"
	"github.com/sunfish-shogi/bufseekio"
)

type sizeChange struct {
	diff int64
}

func main() {
	if len(os.Args) != 3 {
		println("Usage: borkmp4 INPUT.mp4 OUTPUT.mp4")
		return
	}

	inputPath := os.Args[1]
	outputPath := os.Args[2]

	err := editFile(inputPath, outputPath)
	if err != nil {
		log.Fatalln("Error:", err)
	}
}

// This function is adapted from the example at
// https://github.com/abema/go-mp4/blob/master/cmd/mp4tool/internal/edit/edit.go
// Copyright (c) 2020 AbemaTV; MIT License

func editFile(inputPath, outputPath string) error {
	inputFile, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer inputFile.Close()

	outputFile, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outputFile.Close()

	var sizeDiff int64 = 0
	var chunkBoxOffsets []int64
	var chunkBoxWriteOffsets []int64

	r := bufseekio.NewReadSeeker(inputFile, 128*1024, 4)
	w := mp4.NewWriter(outputFile)
	_, err = mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (any, error) {
		if h.BoxInfo.Type == mp4.BoxTypeMdat() && sizeDiff > 0 {
			readPos, err := r.Seek(0, io.SeekCurrent)
			if err != nil {
				return nil, err
			}
			writePos, err := w.Seek(0, io.SeekCurrent)
			if err != nil {
				return nil, err
			}

			fmt.Printf("will add %v to chunk offsets\n", sizeDiff)

			for idx, offset := range chunkBoxOffsets {
				_, err := r.Seek(offset, io.SeekStart)
				if err != nil {
					return nil, err
				}

				boxInfo, err := mp4.ReadBoxInfo(r)
				if err != nil {
					return nil, err
				}
				_, err = boxInfo.SeekToPayload(r)
				if err != nil {
					return nil, err
				}

				box, _, err := mp4.UnmarshalAny(r, boxInfo.Type, boxInfo.Size-boxInfo.HeaderSize, mp4.Context{})
				if err != nil {
					return nil, err
				}

				switch boxInfo.Type {
				case mp4.BoxTypeStco():
					stco := box.(*mp4.Stco)
					for idx, offset := range stco.ChunkOffset {
						stco.ChunkOffset[idx] = uint32(int64(offset) + sizeDiff)
					}
				case mp4.BoxTypeCo64():
					co64 := box.(*mp4.Co64)
					for idx, offset := range co64.ChunkOffset {
						co64.ChunkOffset[idx] = uint64(int64(offset) + sizeDiff)
					}
				default:
					return nil, fmt.Errorf("unknown chunk box type %v", boxInfo.Type)
				}

				_, err = w.Seek(chunkBoxWriteOffsets[idx]+int64(boxInfo.HeaderSize), io.SeekStart)
				if err != nil {
					return nil, err
				}

				if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
					return nil, err
				}

				fmt.Printf("rewrote at %v\n", offset)
			}

			_, err = r.Seek(readPos, io.SeekStart)
			if err != nil {
				return nil, err
			}
			_, err = w.Seek(writePos, io.SeekStart)
			if err != nil {
				return nil, err
			}
		}

		if !h.BoxInfo.IsSupportedType() || h.BoxInfo.Type == mp4.BoxTypeMdat() {
			// copy all data
			return nil, w.CopyBox(r, &h.BoxInfo)
		}

		// read payload
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}

		// edit ESDS field
		switch h.BoxInfo.Type {
		case mp4.BoxTypeEsds():
			esds := box.(*mp4.Esds)
			for idx, descr := range esds.Descriptors {
				if descr.Tag == mp4.DecSpecificInfoTag {
					var objType = descr.Data[0] >> 3
					if objType != 2 {
						println("esds audio object type was not AAC LC, skipping")
						continue
					}
					var freqIndex = ((descr.Data[0] & 0x7) << 1) + (descr.Data[1] >> 7)
					if freqIndex > 0xC {
						println("custom frequency indices are not supported, skipping")
						continue
					}
					var channelConfig = (descr.Data[1] & 0x78) >> 3
					if channelConfig == 0 {
						println("AOT specific config not supported, skipping")
						continue
					}

					var audConf [4]byte
					audConf[0] = 5 << 3          // AAC SBR object type (5 bit uint)
					audConf[0] += freqIndex >> 1 // freqIndex (4 bit uint)
					audConf[1] = (freqIndex & 0x1) << 7
					audConf[1] += channelConfig << 3 // channelConfig (4 bit uint)
					audConf[1] += freqIndex >> 1     // extFreqIndex
					audConf[2] = (freqIndex & 0x1) << 7
					audConf[2] += 2 << 2 //  AAC LC object type (extension)

					esds.Descriptors[idx].Data = audConf[:]
					esds.Descriptors[idx].Size = uint32(len(audConf))
					println("Breaking your esds atom for you, you're welcome!")
				}
			}
		case mp4.BoxTypeStco(), mp4.BoxTypeCo64():
			pos, err := w.Seek(0, io.SeekCurrent)
			if err != nil {
				return nil, err
			}
			chunkBoxOffsets = append(chunkBoxOffsets, int64(h.BoxInfo.Offset))
			chunkBoxWriteOffsets = append(chunkBoxWriteOffsets, pos)
		}

		// write header
		_, err = w.StartBox(&h.BoxInfo)
		if err != nil {
			return nil, err
		}

		// write payload
		if _, err := mp4.Marshal(w, box, h.BoxInfo.Context); err != nil {
			return nil, err
		}

		// expand all of offsprings
		results, err := h.Expand()
		if err != nil {
			return nil, err
		}

		var childrenSizeDiff int64 = 0

		for _, result := range results {
			if result == nil {
				continue
			}
			if sizeChange, ok := result.(*sizeChange); ok {
				childrenSizeDiff += sizeChange.diff
			}
		}

		// rewrite box size
		newBoxInfo, err := w.EndBox()
		if err != nil {
			return nil, err
		}

		localSizeDiff := int64(newBoxInfo.Size - h.BoxInfo.Size)
		if localSizeDiff-childrenSizeDiff > 0 {
			fmt.Printf("box type: %v, size difference: %v\n", h.BoxInfo.Type, localSizeDiff)
			sizeDiff += localSizeDiff
		}

		if localSizeDiff > 0 {
			return &sizeChange{diff: localSizeDiff}, nil
		}

		return nil, nil
	})
	return err
}
