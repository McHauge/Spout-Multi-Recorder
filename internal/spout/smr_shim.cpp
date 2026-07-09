// smr_shim.cpp - plain C interface over the SpoutDX C++ class for cgo.
#include "SpoutDX.h"
#include "smr_shim.h"

struct SmrReceiver {
	spoutDX dx;
	unsigned int w = 0;
	unsigned int h = 0;
};

extern "C" {

void* smr_create(const char* sendername) {
	SmrReceiver* r = new (std::nothrow) SmrReceiver();
	if (!r) return nullptr;
	r->dx.DisableSpoutLog();
	if (!r->dx.OpenDirectX11()) {
		delete r;
		return nullptr;
	}
	if (sendername && sendername[0] != 0)
		r->dx.SetReceiverName(sendername);
	return (void*)r;
}

void smr_destroy(void* h) {
	if (!h) return;
	SmrReceiver* r = (SmrReceiver*)h;
	r->dx.ReleaseReceiver();
	r->dx.CloseDirectX11();
	delete r;
}

int smr_receive(void* h, unsigned char* pixels, int invert) {
	if (!h) return 0;
	SmrReceiver* r = (SmrReceiver*)h;
	int flags = 0;
	// ReceiveImage keeps trying to (re)connect to the named sender on every
	// call and returns false while there is no sender.
	if (r->dx.ReceiveImage(pixels, r->w, r->h, false, invert != 0)) {
		if (r->dx.IsUpdated()) {
			// Sender created or changed size/format. Buffer must be resized
			// by the caller before the next call. Pixels were not written.
			r->w = r->dx.GetSenderWidth();
			r->h = r->dx.GetSenderHeight();
			flags |= SMR_UPDATED;
		} else if (r->dx.IsFrameNew()) {
			flags |= SMR_NEWFRAME;
		}
	}
	if (r->dx.IsConnected())
		flags |= SMR_CONNECTED;
	return flags;
}

unsigned int smr_width(void* h) {
	return h ? ((SmrReceiver*)h)->w : 0;
}

unsigned int smr_height(void* h) {
	return h ? ((SmrReceiver*)h)->h : 0;
}

unsigned int smr_format(void* h) {
	return h ? (unsigned int)((SmrReceiver*)h)->dx.GetSenderFormat() : 0;
}

unsigned int smr_sender_fps_x1000(void* h) {
	if (!h) return 0;
	double fps = ((SmrReceiver*)h)->dx.GetSenderFps();
	if (fps < 0.0) fps = 0.0;
	return (unsigned int)(fps * 1000.0);
}

// A single lightweight instance used only for name enumeration.
static spoutSenderNames g_names;

int smr_sender_count(void) {
	return g_names.GetSenderCount();
}

int smr_sender_name(int index, char* name, int maxlen) {
	if (!name || maxlen <= 0) return 0;
	name[0] = 0;
	std::set<std::string> senders;
	if (!g_names.GetSenderNames(&senders)) return 0;
	if (index < 0 || index >= (int)senders.size()) return 0;
	int i = 0;
	for (const std::string& s : senders) {
		if (i == index) {
			strncpy_s(name, (size_t)maxlen, s.c_str(), _TRUNCATE);
			return 1;
		}
		i++;
	}
	return 0;
}

} // extern "C"
