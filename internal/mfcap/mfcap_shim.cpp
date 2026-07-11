// mfcap_shim.cpp - Media Foundation UVC webcam capture behind a plain-C API.
//
// Threading model: every device runs on its own native worker thread which
// owns the COM apartment (MTA) and the IMFSourceReader. Frames are delivered
// asynchronously on MF work-queue threads (IMFSourceReaderCallback) and copied
// into a lock-protected mailbox. The cgo caller only ever reads that mailbox
// (mfcap_latest), so it never has to initialise or enter a COM apartment.

#include "mfcap_shim.h"

#include <windows.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfreadwrite.h>
#include <mferror.h>
#include <mfobjects.h>

#include <string>
#include <vector>
#include <mutex>
#include <thread>

// (Import libraries are provided via cgo LDFLAGS in mfcap.go, not #pragma.)

// ---------------------------------------------------------------------------
// One-time Media Foundation startup.
// ---------------------------------------------------------------------------
static std::once_flag g_startOnce;
static bool g_started = false;

static void startupMF() {
    std::call_once(g_startOnce, []() {
        HRESULT hr = MFStartup(MF_VERSION, MFSTARTUP_LITE);
        g_started = SUCCEEDED(hr);
    });
}

int mfcap_available(void) {
    startupMF();
    return g_started ? 1 : 0;
}

// ---------------------------------------------------------------------------
// Device enumeration (results cached for mfcap_device_name/link).
// ---------------------------------------------------------------------------
static std::mutex g_enumMu;
static std::vector<std::wstring> g_names;
static std::vector<std::wstring> g_links;

static int utf8Copy(const std::wstring& w, char* buf, int maxlen) {
    if (!buf || maxlen <= 0) return -1;
    int n = WideCharToMultiByte(CP_UTF8, 0, w.c_str(), (int)w.size(),
                                buf, maxlen - 1, nullptr, nullptr);
    if (n < 0) n = 0;
    buf[n] = 0;
    return n;
}

static std::wstring utf8ToWide(const char* s) {
    if (!s) return L"";
    int wlen = MultiByteToWideChar(CP_UTF8, 0, s, -1, nullptr, 0);
    if (wlen <= 0) return L"";
    std::wstring w(wlen, 0);
    MultiByteToWideChar(CP_UTF8, 0, s, -1, &w[0], wlen);
    if (!w.empty() && w.back() == 0) w.pop_back();
    return w;
}

// createSource builds a Media Foundation device source for a symbolic link.
static IMFMediaSource* createSource(const std::wstring& symlink) {
    IMFAttributes* devAttr = nullptr;
    IMFMediaSource* source = nullptr;
    if (SUCCEEDED(MFCreateAttributes(&devAttr, 2))) {
        devAttr->SetGUID(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE,
                         MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID);
        devAttr->SetString(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_SYMBOLIC_LINK,
                           symlink.c_str());
        MFCreateDeviceSource(devAttr, &source);
        devAttr->Release();
    }
    return source;
}

// enumerate runs on a dedicated thread so it owns a clean COM apartment.
static int enumerate() {
    std::vector<std::wstring> names, links;
    HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
    bool didInit = SUCCEEDED(hr);

    IMFAttributes* attr = nullptr;
    hr = MFCreateAttributes(&attr, 1);
    if (SUCCEEDED(hr)) {
        attr->SetGUID(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE,
                      MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID);
        IMFActivate** devices = nullptr;
        UINT32 count = 0;
        hr = MFEnumDeviceSources(attr, &devices, &count);
        if (SUCCEEDED(hr)) {
            for (UINT32 i = 0; i < count; i++) {
                WCHAR* name = nullptr;
                WCHAR* link = nullptr;
                UINT32 nlen = 0, llen = 0;
                devices[i]->GetAllocatedString(MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME, &name, &nlen);
                devices[i]->GetAllocatedString(
                    MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_SYMBOLIC_LINK, &link, &llen);
                names.push_back(name ? std::wstring(name) : L"");
                links.push_back(link ? std::wstring(link) : L"");
                if (name) CoTaskMemFree(name);
                if (link) CoTaskMemFree(link);
                devices[i]->Release();
            }
            if (devices) CoTaskMemFree(devices);
        }
        attr->Release();
    }
    if (didInit) CoUninitialize();

    std::lock_guard<std::mutex> lk(g_enumMu);
    g_names = std::move(names);
    g_links = std::move(links);
    return (int)g_names.size();
}

int mfcap_enum(void) {
    if (!mfcap_available()) return -1;
    int n = -1;
    std::thread t([&]() { n = enumerate(); });
    t.join();
    return n;
}

int mfcap_device_name(int index, char* buf, int maxlen) {
    std::lock_guard<std::mutex> lk(g_enumMu);
    if (index < 0 || index >= (int)g_names.size()) return -1;
    return utf8Copy(g_names[index], buf, maxlen);
}

int mfcap_device_link(int index, char* buf, int maxlen) {
    std::lock_guard<std::mutex> lk(g_enumMu);
    if (index < 0 || index >= (int)g_links.size()) return -1;
    return utf8Copy(g_links[index], buf, maxlen);
}

// ---------------------------------------------------------------------------
// Mode enumeration (distinct width/height/fps triples, cached for mfcap_mode).
// ---------------------------------------------------------------------------
struct ModeInfo {
    unsigned w, h, fpsx;
};
static std::mutex g_modeMu;
static std::vector<ModeInfo> g_modes;

static int enumModes(const std::wstring& symlink) {
    std::vector<ModeInfo> modes;
    IMFMediaSource* source = createSource(symlink);
    if (!source) return -1;
    IMFSourceReader* reader = nullptr;
    if (SUCCEEDED(MFCreateSourceReaderFromMediaSource(source, nullptr, &reader)) && reader) {
        for (DWORD i = 0;; i++) {
            IMFMediaType* type = nullptr;
            HRESULT hr = reader->GetNativeMediaType(MF_SOURCE_READER_FIRST_VIDEO_STREAM, i, &type);
            if (hr == MF_E_NO_MORE_TYPES || FAILED(hr)) break;
            UINT32 w = 0, h = 0;
            MFGetAttributeSize(type, MF_MT_FRAME_SIZE, &w, &h);
            UINT32 num = 0, den = 0;
            unsigned fpsx = 0;
            if (SUCCEEDED(MFGetAttributeRatio(type, MF_MT_FRAME_RATE, &num, &den)) && den)
                fpsx = (unsigned)((double)num / (double)den * 1000.0 + 0.5);
            bool dup = false;
            for (const auto& m : modes)
                if (m.w == w && m.h == h && m.fpsx == fpsx) { dup = true; break; }
            if (!dup && w > 0 && h > 0) modes.push_back({w, h, fpsx});
            type->Release();
        }
        reader->Release();
    }
    source->Shutdown();
    source->Release();
    std::lock_guard<std::mutex> lk(g_modeMu);
    g_modes = std::move(modes);
    return (int)g_modes.size();
}

int mfcap_enum_modes(const char* symlink) {
    if (!mfcap_available() || !symlink) return -1;
    std::wstring w = utf8ToWide(symlink);
    int n = -1;
    std::thread t([&]() {
        HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
        bool init = SUCCEEDED(hr);
        n = enumModes(w);
        if (init) CoUninitialize();
    });
    t.join();
    return n;
}

int mfcap_mode(int index, unsigned int* w, unsigned int* h, unsigned int* fps_x1000) {
    std::lock_guard<std::mutex> lk(g_modeMu);
    if (index < 0 || index >= (int)g_modes.size()) return 0;
    if (w) *w = g_modes[index].w;
    if (h) *h = g_modes[index].h;
    if (fps_x1000) *fps_x1000 = g_modes[index].fpsx;
    return 1;
}

// ---------------------------------------------------------------------------
// Capture object + async source-reader callback.
// ---------------------------------------------------------------------------
struct Capture {
    SRWLOCK lock;
    std::vector<uint8_t> frame; // top-down BGRA, width*height*4
    int width = 0;
    int height = 0;
    unsigned int fpsx1000 = 0;
    bool hasNew = false;
    bool connected = false;
    bool lost = false;

    std::wstring symlink;
    unsigned wantW = 0, wantH = 0, wantFps = 0; // desired mode (0 = auto)
    HANDLE stopEvent = nullptr;
    HANDLE ready = nullptr; // signalled once open succeeds/fails
    bool openOK = false;
    std::thread worker;

    IMFSourceReader* reader = nullptr;
    LONG stopping = 0;
};

class SourceReaderCB : public IMFSourceReaderCallback {
public:
    explicit SourceReaderCB(Capture* c) : cap_(c) {}

    // IUnknown
    STDMETHODIMP QueryInterface(REFIID iid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (iid == IID_IUnknown || iid == IID_IMFSourceReaderCallback) {
            *ppv = static_cast<IMFSourceReaderCallback*>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    STDMETHODIMP_(ULONG) AddRef() override { return InterlockedIncrement(&ref_); }
    STDMETHODIMP_(ULONG) Release() override {
        LONG r = InterlockedDecrement(&ref_);
        if (r == 0) delete this;
        return r;
    }

    STDMETHODIMP OnReadSample(HRESULT hrStatus, DWORD streamIndex, DWORD streamFlags,
                              LONGLONG timestamp, IMFSample* sample) override {
        (void)streamIndex;
        (void)timestamp;
        if (FAILED(hrStatus) || (streamFlags & MF_SOURCE_READERF_ERROR) ||
            (streamFlags & MF_SOURCE_READERF_ENDOFSTREAM)) {
            AcquireSRWLockExclusive(&cap_->lock);
            cap_->lost = true;
            cap_->connected = false;
            ReleaseSRWLockExclusive(&cap_->lock);
            return S_OK; // do not re-issue
        }
        if (sample) copySample(sample);
        // Keep the pipeline going unless we are tearing down.
        if (!InterlockedCompareExchange(&cap_->stopping, 0, 0) && cap_->reader) {
            cap_->reader->ReadSample(MF_SOURCE_READER_FIRST_VIDEO_STREAM, 0,
                                     nullptr, nullptr, nullptr, nullptr);
        }
        return S_OK;
    }
    STDMETHODIMP OnEvent(DWORD, IMFMediaEvent*) override { return S_OK; }
    STDMETHODIMP OnFlush(DWORD) override { return S_OK; }

private:
    void copySample(IMFSample* sample) {
        IMFMediaBuffer* buf = nullptr;
        if (FAILED(sample->ConvertToContiguousBuffer(&buf)) || !buf) return;

        int w = cap_->width, h = cap_->height;
        if (w <= 0 || h <= 0) { buf->Release(); return; }
        const int rowBytes = w * 4;

        IMF2DBuffer* b2d = nullptr;
        if (SUCCEEDED(buf->QueryInterface(IID_IMF2DBuffer, (void**)&b2d)) && b2d) {
            BYTE* scan0 = nullptr;
            LONG stride = 0;
            if (SUCCEEDED(b2d->Lock2D(&scan0, &stride))) {
                AcquireSRWLockExclusive(&cap_->lock);
                cap_->frame.resize((size_t)rowBytes * h);
                for (int y = 0; y < h; y++) {
                    memcpy(cap_->frame.data() + (size_t)y * rowBytes,
                           scan0 + (LONGLONG)y * stride, rowBytes);
                }
                cap_->hasNew = true;
                cap_->connected = true;
                ReleaseSRWLockExclusive(&cap_->lock);
                b2d->Unlock2D();
            }
            b2d->Release();
        } else {
            BYTE* data = nullptr;
            DWORD maxLen = 0, curLen = 0;
            if (SUCCEEDED(buf->Lock(&data, &maxLen, &curLen))) {
                const int need = rowBytes * h;
                if ((int)curLen >= need) {
                    AcquireSRWLockExclusive(&cap_->lock);
                    cap_->frame.resize((size_t)need);
                    memcpy(cap_->frame.data(), data, need);
                    cap_->hasNew = true;
                    cap_->connected = true;
                    ReleaseSRWLockExclusive(&cap_->lock);
                }
                buf->Unlock();
            }
        }
        buf->Release();
    }

    LONG ref_ = 1;
    Capture* cap_;
};

// Choose a native media type and set it, reporting the chosen dims/fps.
//   wantW,wantH > 0 : that exact resolution, fps nearest wantFps (0 = highest).
//   else wantFps > 0: highest resolution that reaches wantFps (best effort if
//                     none do); among those the fps closest to the target.
//   else            : highest resolution at >=30fps.
static HRESULT chooseMode(IMFSourceReader* reader, unsigned wantW, unsigned wantH, unsigned wantFps,
                          int* outW, int* outH, unsigned int* outFpsX1000) {
    IMFMediaType* best = nullptr;
    int bestW = 0, bestH = 0;
    double bestFps = 0;
    long long bestScore = -1;

    for (DWORD i = 0;; i++) {
        IMFMediaType* type = nullptr;
        HRESULT hr = reader->GetNativeMediaType(MF_SOURCE_READER_FIRST_VIDEO_STREAM, i, &type);
        if (hr == MF_E_NO_MORE_TYPES || FAILED(hr)) break;

        UINT32 w = 0, h = 0;
        MFGetAttributeSize(type, MF_MT_FRAME_SIZE, &w, &h);
        UINT32 num = 0, den = 0;
        double fps = 0;
        if (SUCCEEDED(MFGetAttributeRatio(type, MF_MT_FRAME_RATE, &num, &den)) && den) {
            fps = (double)num / (double)den;
        }
        unsigned fpsx = (unsigned)(fps * 1000.0 + 0.5);
        long long area = (long long)w * (long long)h;

        long long score;
        if (wantW > 0 && wantH > 0) {
            if (w != wantW || h != wantH) { type->Release(); continue; }
            score = (wantFps > 0) ? ((long long)1e12 - llabs((long long)fpsx - (long long)wantFps))
                                  : (long long)fpsx;
        } else if (wantFps > 0) {
            bool meets = (long long)fpsx + 500 >= (long long)wantFps; // 0.5fps tolerance
            if (meets)
                score = (long long)1e15 + area * 1000000LL - llabs((long long)fpsx - (long long)wantFps);
            else
                score = (long long)fpsx * 1000000LL + area; // none meet: closest fps, then area
        } else {
            score = area + (fps >= 29.97 ? (long long)5e9 : 0) + (long long)(fps * 100.0);
        }

        if (score > bestScore) {
            bestScore = score;
            bestW = (int)w;
            bestH = (int)h;
            bestFps = fps;
            if (best) best->Release();
            best = type;
            best->AddRef();
        }
        type->Release();
    }
    // Requested resolution not offered: fall back to auto for the target fps.
    if (!best && wantW > 0) {
        return chooseMode(reader, 0, 0, wantFps, outW, outH, outFpsX1000);
    }
    if (!best) return E_FAIL;

    HRESULT hr = reader->SetCurrentMediaType(MF_SOURCE_READER_FIRST_VIDEO_STREAM, nullptr, best);
    best->Release();
    if (FAILED(hr)) return hr;

    // Ask the reader for BGRA (RGB32) output; advanced video processing inserts
    // any needed decoder/converter.
    IMFMediaType* out = nullptr;
    hr = MFCreateMediaType(&out);
    if (SUCCEEDED(hr)) {
        out->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        out->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_RGB32);
        hr = reader->SetCurrentMediaType(MF_SOURCE_READER_FIRST_VIDEO_STREAM, nullptr, out);
        out->Release();
    }
    if (FAILED(hr)) return hr;

    *outW = bestW;
    *outH = bestH;
    *outFpsX1000 = (unsigned int)(bestFps * 1000.0 + 0.5);
    return S_OK;
}

static void workerMain(Capture* cap) {
    HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
    bool didInit = SUCCEEDED(hr);

    SourceReaderCB* cb = nullptr;

    // Build the device source from the symbolic link.
    IMFMediaSource* source = createSource(cap->symlink);
    hr = source ? S_OK : E_FAIL;

    if (SUCCEEDED(hr) && source) {
        cb = new SourceReaderCB(cap);
        IMFAttributes* rdAttr = nullptr;
        if (SUCCEEDED(MFCreateAttributes(&rdAttr, 2))) {
            rdAttr->SetUINT32(MF_SOURCE_READER_ENABLE_ADVANCED_VIDEO_PROCESSING, TRUE);
            rdAttr->SetUnknown(MF_SOURCE_READER_ASYNC_CALLBACK, cb);
            hr = MFCreateSourceReaderFromMediaSource(source, rdAttr, &cap->reader);
            rdAttr->Release();
        } else {
            hr = E_FAIL;
        }
    }

    if (SUCCEEDED(hr) && cap->reader) {
        int w = 0, h = 0;
        unsigned int fps = 0;
        hr = chooseMode(cap->reader, cap->wantW, cap->wantH, cap->wantFps, &w, &h, &fps);
        if (SUCCEEDED(hr)) {
            AcquireSRWLockExclusive(&cap->lock);
            cap->width = w;
            cap->height = h;
            cap->fpsx1000 = fps;
            ReleaseSRWLockExclusive(&cap->lock);
        }
    }

    cap->openOK = SUCCEEDED(hr) && cap->reader != nullptr;
    if (cap->openOK) {
        // Kick off asynchronous delivery.
        cap->reader->ReadSample(MF_SOURCE_READER_FIRST_VIDEO_STREAM, 0,
                                nullptr, nullptr, nullptr, nullptr);
    }
    SetEvent(cap->ready);

    if (cap->openOK) {
        WaitForSingleObject(cap->stopEvent, INFINITE);
    }

    // Teardown: stop re-issuing, flush pending callbacks, release.
    InterlockedExchange(&cap->stopping, 1);
    if (cap->reader) {
        cap->reader->Flush(MF_SOURCE_READER_FIRST_VIDEO_STREAM);
        cap->reader->Release();
        cap->reader = nullptr;
    }
    if (source) {
        source->Shutdown();
        source->Release();
    }
    if (cb) cb->Release();
    if (didInit) CoUninitialize();
}

void* mfcap_open(const char* symlink, unsigned int wantW, unsigned int wantH, unsigned int wantFpsX1000) {
    if (!mfcap_available() || !symlink) return nullptr;

    Capture* cap = new Capture();
    InitializeSRWLock(&cap->lock);
    cap->stopEvent = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    cap->ready = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    cap->symlink = utf8ToWide(symlink);
    cap->wantW = wantW;
    cap->wantH = wantH;
    cap->wantFps = wantFpsX1000;

    cap->worker = std::thread(workerMain, cap);
    WaitForSingleObject(cap->ready, 10000); // bounded wait for open

    if (!cap->openOK) {
        // Open failed: signal the (parked or exiting) worker and reap it.
        SetEvent(cap->stopEvent);
        if (cap->worker.joinable()) cap->worker.join();
        CloseHandle(cap->stopEvent);
        CloseHandle(cap->ready);
        delete cap;
        return nullptr;
    }
    return cap;
}

void mfcap_close(void* h) {
    Capture* cap = (Capture*)h;
    if (!cap) return;
    SetEvent(cap->stopEvent);
    if (cap->worker.joinable()) cap->worker.join();
    CloseHandle(cap->stopEvent);
    CloseHandle(cap->ready);
    delete cap;
}

unsigned int mfcap_width(void* h) {
    Capture* cap = (Capture*)h;
    if (!cap) return 0;
    AcquireSRWLockShared(&cap->lock);
    unsigned int v = (unsigned int)cap->width;
    ReleaseSRWLockShared(&cap->lock);
    return v;
}

unsigned int mfcap_height(void* h) {
    Capture* cap = (Capture*)h;
    if (!cap) return 0;
    AcquireSRWLockShared(&cap->lock);
    unsigned int v = (unsigned int)cap->height;
    ReleaseSRWLockShared(&cap->lock);
    return v;
}

unsigned int mfcap_fps_x1000(void* h) {
    Capture* cap = (Capture*)h;
    if (!cap) return 0;
    AcquireSRWLockShared(&cap->lock);
    unsigned int v = cap->fpsx1000;
    ReleaseSRWLockShared(&cap->lock);
    return v;
}

int mfcap_latest(void* h, unsigned char* dst, int dstcap) {
    Capture* cap = (Capture*)h;
    if (!cap) return 0;
    int flags = 0;
    AcquireSRWLockExclusive(&cap->lock);
    if (cap->lost) flags |= MFCAP_LOST;
    if (cap->connected) flags |= MFCAP_CONNECTED;
    if (cap->hasNew && dst && dstcap > 0) {
        int need = cap->width * cap->height * 4;
        if (need > 0 && (int)cap->frame.size() >= need && dstcap >= need) {
            memcpy(dst, cap->frame.data(), need);
            cap->hasNew = false;
            flags |= MFCAP_NEWFRAME;
        }
    }
    ReleaseSRWLockExclusive(&cap->lock);
    return flags;
}
