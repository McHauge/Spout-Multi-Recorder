// mfcap_shim.h - plain C interface over a Media Foundation webcam (UVC)
// capture, for cgo. Mirrors the style of the Spout shim (smr_shim.h): the Go
// side polls a latest-frame mailbox; all COM/MF work happens on shim-owned
// threads so cgo callers never touch a COM apartment.
#ifndef MFCAP_SHIM_H
#define MFCAP_SHIM_H

#ifdef __cplusplus
extern "C" {
#endif

// Flags returned by mfcap_latest.
#define MFCAP_CONNECTED 1 // a device is opened and delivering frames
#define MFCAP_NEWFRAME  4 // a new frame was copied into dst this call
#define MFCAP_LOST      8 // device error/unplug: caller should close and reopen

// Initialise Media Foundation (idempotent). Returns 1 if available, else 0.
int mfcap_available(void);

// (Re)enumerate video capture devices into an internal list. Returns the
// device count, or -1 on error. Names/links are read with mfcap_device_*.
int mfcap_enum(void);
// Copy the UTF-8 friendly name / stable symbolic link of device `index` into
// buf (maxlen incl. NUL). Returns bytes written (excl. NUL), or -1.
int mfcap_device_name(int index, char* buf, int maxlen);
int mfcap_device_link(int index, char* buf, int maxlen);

// Enumerate the distinct video modes of the device with the given symbolic
// link into an internal list. Returns the mode count, or -1 on error.
int mfcap_enum_modes(const char* symlink);
// Read mode `index`: writes width, height and fps*1000. Returns 1 on success.
int mfcap_mode(int index, unsigned int* w, unsigned int* h, unsigned int* fps_x1000);

// Open the device with the given UTF-8 symbolic link. Returns a handle, or
// NULL on failure. Frames are converted to BGRA. Mode selection:
//   wantW>0 && wantH>0 : that exact resolution, nearest available fps.
//   else wantFpsX1000>0: highest resolution whose fps reaches wantFpsX1000.
//   else               : highest resolution at >=30fps.
void* mfcap_open(const char* symlink, unsigned int wantW, unsigned int wantH, unsigned int wantFpsX1000);
// Close a handle opened with mfcap_open (safe to call once, from any thread).
void  mfcap_close(void* h);

unsigned int mfcap_width(void* h);
unsigned int mfcap_height(void* h);
unsigned int mfcap_fps_x1000(void* h);

// Copy the newest frame (top-down BGRA, width*height*4 bytes) into dst if one
// arrived since the last call. Never blocks. Returns MFCAP_* flags.
int mfcap_latest(void* h, unsigned char* dst, int dstcap);

#ifdef __cplusplus
}
#endif

#endif
