// dl_shim.h - plain C interface over the DeckLink COM capture API, for cgo.
// The Go side polls a latest-frame mailbox and drains an audio ring; all COM
// work happens on shim-owned threads.
#ifndef DL_SHIM_H
#define DL_SHIM_H

#ifdef __cplusplus
extern "C" {
#endif

// Flags returned by dl_video_latest.
#define DL_CONNECTED 1 // an input signal is present
#define DL_NEWFRAME  4 // a new frame was copied into dst this call

// Returns 1 if the DeckLink driver is installed (the iterator can be created).
int dl_available(void);

// (Re)enumerate DeckLink devices. Returns the count, or -1 on error.
int dl_device_count(void);
// Copy the UTF-8 display name of device `index` into buf (maxlen incl. NUL).
// Returns bytes written (excl. NUL), or -1.
int dl_device_name(int index, char* buf, int maxlen);

// Open the input of the device with the given UTF-8 display name. Video input
// uses automatic format detection; audio is enabled up to the card's maximum
// (capped at 16 channels). Returns a handle, or NULL on failure.
void* dl_open(const char* name);
void  dl_close(void* h);

unsigned int dl_width(void* h);
unsigned int dl_height(void* h);
// Channels of the enabled audio input.
int dl_audio_channels(void* h);

// Copy the newest BGRA frame (width*height*4 bytes) into dst if one arrived.
// Never blocks. Returns DL_* flags.
int dl_video_latest(void* h, unsigned char* dst, int dstcap);
// Drain up to cap bytes of interleaved s16le 48kHz audio into dst. Returns the
// number of bytes written; *channels is set to the audio channel count.
int dl_audio_read(void* h, unsigned char* dst, int cap, int* channels);

#ifdef __cplusplus
}
#endif

#endif
