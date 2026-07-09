// smr_shim.h - plain C interface over the SpoutDX C++ class for cgo.
#ifndef SMR_SHIM_H
#define SMR_SHIM_H

#ifdef __cplusplus
extern "C" {
#endif

// Result flags returned by smr_receive
#define SMR_CONNECTED 1 // receiver is connected to the sender
#define SMR_UPDATED 2   // sender size/format changed; pixels NOT written; re-query dims and resize buffer
#define SMR_NEWFRAME 4  // a new frame was copied into the pixel buffer

// Create a receiver bound to a specific sender name.
void* smr_create(const char* sendername);

// Destroy the receiver and free resources.
void smr_destroy(void* h);

// Poll the sender. If connected and a new frame is available it is copied
// into pixels (which must be width*height*4 bytes, matching smr_width/height).
// Pass invert=1 to flip vertically. Returns SMR_* flags.
int smr_receive(void* h, unsigned char* pixels, int invert);

unsigned int smr_width(void* h);
unsigned int smr_height(void* h);
// DXGI_FORMAT of the sender texture (87=BGRA8, 28=RGBA8, ...)
unsigned int smr_format(void* h);
// Sender fps as reported by the frame counter (float*1000)
unsigned int smr_sender_fps_x1000(void* h);

// Global sender enumeration (usable without a receiver).
int smr_sender_count(void);
// Copies sender name at index into name (maxlen incl. NUL). Returns 1 on success.
int smr_sender_name(int index, char* name, int maxlen);

#ifdef __cplusplus
}
#endif

#endif
