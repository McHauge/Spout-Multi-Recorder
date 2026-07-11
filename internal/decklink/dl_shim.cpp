// dl_shim.cpp - Blackmagic DeckLink SDI/HDMI capture behind a plain-C API.
//
// Threading: each open device runs on its own native worker thread that owns
// the COM apartment (MTA), the IDeckLinkInput and the conversion object. Frames
// and audio are delivered on DeckLink's callback threads and copied into a
// lock-protected mailbox (video) and ring (audio). The cgo caller only reads
// those (dl_video_latest / dl_audio_read), never touching COM.

#define NOMINMAX

#include "dl_com.h"
#include "dl_shim.h"

#include <string>
#include <vector>
#include <mutex>
#include <thread>
#include <algorithm>

// GUIDs needed for QueryInterface responses / device querying, taken from the
// installed driver's type library.
static const GUID k_IID_IDeckLink =
    {0xC418FBDD, 0x0587, 0x48ED, {0x8F, 0xE5, 0x64, 0x0F, 0x0A, 0x14, 0xAF, 0x91}};
static const GUID k_IID_IDeckLinkInputCallback =
    {0x3A94F075, 0xC37D, 0x4BA8, {0xBC, 0xC0, 0x1D, 0x77, 0x8C, 0x8F, 0x88, 0x1B}};
static const GUID k_IID_IDeckLinkVideoFrame =
    {0x6502091C, 0x615F, 0x4F51, {0xBA, 0xF6, 0x45, 0xC4, 0x25, 0x6D, 0xD5, 0xB0}};

static std::string bstrToUtf8(BSTR b) {
    if (!b) return "";
    int len = (int)SysStringLen(b);
    int n = WideCharToMultiByte(CP_UTF8, 0, b, len, nullptr, 0, nullptr, nullptr);
    std::string s(n, 0);
    if (n > 0) WideCharToMultiByte(CP_UTF8, 0, b, len, &s[0], n, nullptr, nullptr);
    return s;
}

static int utf8Copy(const std::string& s, char* buf, int maxlen) {
    if (!buf || maxlen <= 0) return -1;
    int n = (int)s.size();
    if (n > maxlen - 1) n = maxlen - 1;
    memcpy(buf, s.data(), n);
    buf[n] = 0;
    return n;
}

// ---------------------------------------------------------------------------
// Availability + enumeration.
// ---------------------------------------------------------------------------
static IDeckLinkIterator* createIterator() {
    IDeckLinkIterator* it = nullptr;
    HRESULT hr = CoCreateInstance(CLSID_CDeckLinkIterator, nullptr, CLSCTX_ALL,
                                  IID_IDeckLinkIterator, (void**)&it);
    if (FAILED(hr)) return nullptr;
    return it;
}

// runCOM runs fn on a dedicated thread that owns a fresh MTA apartment.
template <typename F>
static void runCOM(F fn) {
    std::thread t([&]() {
        HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
        bool init = SUCCEEDED(hr);
        fn();
        if (init) CoUninitialize();
    });
    t.join();
}

int dl_available(void) {
    int ok = 0;
    runCOM([&]() {
        IDeckLinkIterator* it = createIterator();
        if (it) { ok = 1; it->Release(); }
    });
    return ok;
}

static std::mutex g_enumMu;
static std::vector<std::string> g_names;

int dl_device_count(void) {
    std::vector<std::string> names;
    runCOM([&]() {
        IDeckLinkIterator* it = createIterator();
        if (!it) return;
        IDeckLink* dl = nullptr;
        while (it->Next(&dl) == S_OK && dl) {
            BSTR name = nullptr;
            if (SUCCEEDED(dl->GetDisplayName(&name)) && name) {
                names.push_back(bstrToUtf8(name));
                SysFreeString(name);
            } else {
                names.push_back("DeckLink");
            }
            dl->Release();
            dl = nullptr;
        }
        it->Release();
    });
    std::lock_guard<std::mutex> lk(g_enumMu);
    g_names = std::move(names);
    return (int)g_names.size();
}

int dl_device_name(int index, char* buf, int maxlen) {
    std::lock_guard<std::mutex> lk(g_enumMu);
    if (index < 0 || index >= (int)g_names.size()) return -1;
    return utf8Copy(g_names[index], buf, maxlen);
}

// ---------------------------------------------------------------------------
// Capture object.
// ---------------------------------------------------------------------------
struct Capture;

// BGRAFrame wraps our own buffer as an IDeckLinkVideoFrame so the DeckLink
// video converter can write BGRA into it.
class BGRAFrame : public IDeckLinkVideoFrame {
public:
    void setup(int w, int h) {
        if (w == width_ && h == height_) return;
        width_ = w;
        height_ = h;
        buf_.resize((size_t)w * h * 4);
    }
    uint8_t* data() { return buf_.data(); }
    size_t size() const { return buf_.size(); }

    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID iid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (iid == IID_IUnknown || iid == k_IID_IDeckLinkVideoFrame) {
            *ppv = static_cast<IDeckLinkVideoFrame*>(this);
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    ULONG STDMETHODCALLTYPE AddRef() override { return 1; }  // owned by Capture
    ULONG STDMETHODCALLTYPE Release() override { return 1; }

    int32_t STDMETHODCALLTYPE GetWidth() override { return width_; }
    int32_t STDMETHODCALLTYPE GetHeight() override { return height_; }
    int32_t STDMETHODCALLTYPE GetRowBytes() override { return width_ * 4; }
    BMDPixelFormat STDMETHODCALLTYPE GetPixelFormat() override { return bmdFormat8BitBGRA; }
    BMDFrameFlags STDMETHODCALLTYPE GetFlags() override { return 0; }
    HRESULT STDMETHODCALLTYPE GetBytes(void** buffer) override {
        if (!buffer) return E_POINTER;
        *buffer = buf_.data();
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE GetTimecode(BMDTimecodeFormat, IDeckLinkTimecode**) override { return E_NOTIMPL; }
    HRESULT STDMETHODCALLTYPE GetAncillaryData(IDeckLinkVideoFrameAncillary**) override { return E_NOTIMPL; }

private:
    int width_ = 0, height_ = 0;
    std::vector<uint8_t> buf_;
};

struct Capture {
    SRWLOCK lock;
    std::vector<uint8_t> frame; // BGRA width*height*4
    int width = 0;
    int height = 0;
    bool hasNew = false;
    bool connected = false;

    std::vector<uint8_t> audio; // interleaved s16le FIFO
    int audioChannels = 2;

    std::string name; // wanted display name (UTF-8)
    HANDLE stopEvent = nullptr;
    HANDLE ready = nullptr;
    bool openOK = false;
    std::thread worker;

    IDeckLinkInput* input = nullptr;
    IDeckLinkVideoConversion* conv = nullptr;
    BGRAFrame dst;
};

static const size_t kAudioRingMax = 48000 * 16 * 2; // ~1s at 16ch s16

class InputCallback : public IDeckLinkInputCallback {
public:
    explicit InputCallback(Capture* c) : cap_(c) {}

    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID iid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (iid == IID_IUnknown || iid == k_IID_IDeckLinkInputCallback) {
            *ppv = static_cast<IDeckLinkInputCallback*>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    ULONG STDMETHODCALLTYPE AddRef() override { return InterlockedIncrement(&ref_); }
    ULONG STDMETHODCALLTYPE Release() override {
        LONG r = InterlockedDecrement(&ref_);
        if (r == 0) delete this;
        return r;
    }

    HRESULT STDMETHODCALLTYPE VideoInputFormatChanged(
        BMDVideoInputFormatChangedEvents, IDeckLinkDisplayMode* newMode,
        BMDDetectedVideoInputFormatFlags flags) override {
        if (!cap_->input || !newMode) return S_OK;
        BMDPixelFormat pf = (flags & bmdDetectedVideoInputRGB444) ? bmdFormat8BitBGRA : bmdFormat8BitYUV;
        cap_->input->PauseStreams();
        cap_->input->EnableVideoInput(newMode->GetDisplayMode(), pf, bmdVideoInputEnableFormatDetection);
        cap_->input->FlushStreams();
        cap_->input->StartStreams();
        return S_OK;
    }

    HRESULT STDMETHODCALLTYPE VideoInputFrameArrived(
        IDeckLinkVideoInputFrame* videoFrame, IDeckLinkAudioInputPacket* audioPacket) override {
        if (videoFrame) {
            BMDFrameFlags ff = videoFrame->GetFlags();
            if (ff & bmdFrameHasNoInputSource) {
                AcquireSRWLockExclusive(&cap_->lock);
                cap_->connected = false;
                ReleaseSRWLockExclusive(&cap_->lock);
            } else {
                int w = videoFrame->GetWidth();
                int h = videoFrame->GetHeight();
                if (w > 0 && h > 0 && cap_->conv) {
                    cap_->dst.setup(w, h);
                    if (SUCCEEDED(cap_->conv->ConvertFrame(videoFrame, &cap_->dst))) {
                        AcquireSRWLockExclusive(&cap_->lock);
                        cap_->frame.assign(cap_->dst.data(), cap_->dst.data() + cap_->dst.size());
                        cap_->width = w;
                        cap_->height = h;
                        cap_->hasNew = true;
                        cap_->connected = true;
                        ReleaseSRWLockExclusive(&cap_->lock);
                    }
                }
            }
        }
        if (audioPacket) {
            int frames = audioPacket->GetSampleFrameCount();
            void* data = nullptr;
            if (frames > 0 && SUCCEEDED(audioPacket->GetBytes(&data)) && data) {
                int bytes = frames * cap_->audioChannels * 2;
                AcquireSRWLockExclusive(&cap_->lock);
                const uint8_t* p = (const uint8_t*)data;
                cap_->audio.insert(cap_->audio.end(), p, p + bytes);
                if (cap_->audio.size() > kAudioRingMax) {
                    size_t drop = cap_->audio.size() - kAudioRingMax;
                    cap_->audio.erase(cap_->audio.begin(), cap_->audio.begin() + drop);
                }
                ReleaseSRWLockExclusive(&cap_->lock);
            }
        }
        return S_OK;
    }

private:
    LONG ref_ = 1;
    Capture* cap_;
};

static void workerMain(Capture* cap) {
    HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
    bool didInit = SUCCEEDED(hr);

    IDeckLink* dev = nullptr;
    InputCallback* cb = nullptr;

    IDeckLinkIterator* it = createIterator();
    if (it) {
        IDeckLink* dl = nullptr;
        while (it->Next(&dl) == S_OK && dl) {
            BSTR name = nullptr;
            std::string dn;
            if (SUCCEEDED(dl->GetDisplayName(&name)) && name) {
                dn = bstrToUtf8(name);
                SysFreeString(name);
            }
            if (dn == cap->name && !dev) {
                dev = dl; // keep
            } else {
                dl->Release();
            }
            dl = nullptr;
        }
        it->Release();
    }

    if (dev) {
        dev->QueryInterface(IID_IDeckLinkInput, (void**)&cap->input);
        // Query card capabilities (best-effort).
        int64_t maxCh = 2;
        int32_t detect = 1;
        IDeckLinkProfileAttributes* attr = nullptr;
        if (SUCCEEDED(dev->QueryInterface(IID_IDeckLinkProfileAttributes, (void**)&attr)) && attr) {
            attr->GetInt(BMDDeckLinkMaximumAudioChannels, &maxCh);
            attr->GetFlag(BMDDeckLinkSupportsInputFormatDetection, &detect);
            attr->Release();
        }
        CoCreateInstance(CLSID_CDeckLinkVideoConversion, nullptr, CLSCTX_ALL,
                         IID_IDeckLinkVideoConversion, (void**)&cap->conv);

        if (cap->input && cap->conv) {
            int ach = (int)std::min<int64_t>(maxCh > 0 ? maxCh : 2, 16);
            if (ach <= 0) ach = 2;
            cap->audioChannels = ach;
            cb = new InputCallback(cap);
            cap->input->SetCallback(cb);
            BMDVideoInputFlags vflags = detect ? bmdVideoInputEnableFormatDetection : bmdVideoInputFlagDefault;
            hr = cap->input->EnableVideoInput(bmdModeHD1080i5994, bmdFormat8BitYUV, vflags);
            if (SUCCEEDED(hr)) {
                cap->input->EnableAudioInput(bmdAudioSampleRate48kHz, bmdAudioSampleType16bitInteger, ach);
                hr = cap->input->StartStreams();
            }
            cap->openOK = SUCCEEDED(hr);
        }
    }

    SetEvent(cap->ready);

    if (cap->openOK) {
        WaitForSingleObject(cap->stopEvent, INFINITE);
    }

    // Teardown.
    if (cap->input) {
        cap->input->StopStreams();
        cap->input->DisableAudioInput();
        cap->input->DisableVideoInput();
        cap->input->SetCallback(nullptr);
        cap->input->Release();
        cap->input = nullptr;
    }
    if (cap->conv) { cap->conv->Release(); cap->conv = nullptr; }
    if (cb) cb->Release();
    if (dev) dev->Release();
    if (didInit) CoUninitialize();
}

void* dl_open(const char* name) {
    if (!name) return nullptr;
    Capture* cap = new Capture();
    InitializeSRWLock(&cap->lock);
    cap->stopEvent = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    cap->ready = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    cap->name = name;
    cap->worker = std::thread(workerMain, cap);
    WaitForSingleObject(cap->ready, 10000);
    if (!cap->openOK) {
        SetEvent(cap->stopEvent);
        if (cap->worker.joinable()) cap->worker.join();
        CloseHandle(cap->stopEvent);
        CloseHandle(cap->ready);
        delete cap;
        return nullptr;
    }
    return cap;
}

void dl_close(void* h) {
    Capture* cap = (Capture*)h;
    if (!cap) return;
    SetEvent(cap->stopEvent);
    if (cap->worker.joinable()) cap->worker.join();
    CloseHandle(cap->stopEvent);
    CloseHandle(cap->ready);
    delete cap;
}

unsigned int dl_width(void* h) {
    Capture* cap = (Capture*)h;
    if (!cap) return 0;
    AcquireSRWLockShared(&cap->lock);
    unsigned int v = (unsigned int)cap->width;
    ReleaseSRWLockShared(&cap->lock);
    return v;
}

unsigned int dl_height(void* h) {
    Capture* cap = (Capture*)h;
    if (!cap) return 0;
    AcquireSRWLockShared(&cap->lock);
    unsigned int v = (unsigned int)cap->height;
    ReleaseSRWLockShared(&cap->lock);
    return v;
}

int dl_audio_channels(void* h) {
    Capture* cap = (Capture*)h;
    if (!cap) return 0;
    return cap->audioChannels;
}

int dl_video_latest(void* h, unsigned char* dst, int dstcap) {
    Capture* cap = (Capture*)h;
    if (!cap) return 0;
    int flags = 0;
    AcquireSRWLockExclusive(&cap->lock);
    if (cap->connected) flags |= DL_CONNECTED;
    if (cap->hasNew && dst && dstcap > 0) {
        int need = cap->width * cap->height * 4;
        if (need > 0 && (int)cap->frame.size() >= need && dstcap >= need) {
            memcpy(dst, cap->frame.data(), need);
            cap->hasNew = false;
            flags |= DL_NEWFRAME;
        }
    }
    ReleaseSRWLockExclusive(&cap->lock);
    return flags;
}

int dl_audio_read(void* h, unsigned char* dst, int cap_bytes, int* channels) {
    Capture* cap = (Capture*)h;
    if (!cap || !dst || cap_bytes <= 0) return 0;
    AcquireSRWLockExclusive(&cap->lock);
    if (channels) *channels = cap->audioChannels;
    int n = (int)std::min<size_t>(cap->audio.size(), (size_t)cap_bytes);
    if (n > 0) {
        memcpy(dst, cap->audio.data(), n);
        cap->audio.erase(cap->audio.begin(), cap->audio.begin() + n);
    }
    ReleaseSRWLockExclusive(&cap->lock);
    return n;
}
