package main

import (
	"log"
	"os"

	"github.com/abema/go-mp4"
	"github.com/sunfish-shogi/bufseekio"
)

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

	r := bufseekio.NewReadSeeker(inputFile, 128*1024, 4)
	w := mp4.NewWriter(outputFile)
	_, err = mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		if !h.BoxInfo.IsSupportedType() || h.BoxInfo.Type == mp4.BoxTypeMdat() {
			// copy all data
			return nil, w.CopyBox(r, &h.BoxInfo)
		}

		// write header
		_, err := w.StartBox(&h.BoxInfo)
		if err != nil {
			return nil, err
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
					esds.Descriptors[idx].Size = 4
					println("Breaking your esds atom for you, you're welcome!")
				}
			}
		}

		// write payload
		if _, err := mp4.Marshal(w, box, h.BoxInfo.Context); err != nil {
			return nil, err
		}

		// expand all of offsprings
		if _, err := h.Expand(); err != nil {
			return nil, err
		}

		// rewrite box size
		_, err = w.EndBox()

		return nil, err
	})
	return err
}
