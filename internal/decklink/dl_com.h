// dl_com.h - minimal Blackmagic DeckLink COM interface declarations for MinGW.
//
// These are NOT the Blackmagic SDK headers. They are a hand-written minimal
// subset covering only the interfaces this shim calls. IIDs, CLSIDs and enum
// constants were read from the installed Desktop Video driver's type library
// (DeckLinkAPI64.dll); the vtable method order follows the published DeckLink
// API. One caveat baked in here: the type library omits IDeckLinkVideoFrame's
// GetBytes (a void** method it cannot represent), so that method is reinserted
// at its real vtable slot (after GetFlags).
#ifndef DL_COM_H
#define DL_COM_H

#include <windows.h>
#include <unknwn.h>
#include <objbase.h>
#include <oleauto.h>
#include <cstdint>

// FourCC-coded enums (typedef'd to uint32_t; values from the typelib).
typedef uint32_t BMDDisplayMode;
typedef uint32_t BMDPixelFormat;
typedef uint32_t BMDVideoInputFlags;
typedef uint32_t BMDAudioSampleRate;
typedef uint32_t BMDAudioSampleType;
typedef uint32_t BMDFrameFlags;
typedef uint32_t BMDVideoInputFormatChangedEvents;
typedef uint32_t BMDDetectedVideoInputFormatFlags;
typedef uint32_t BMDDeckLinkAttributeID;
typedef uint32_t BMDTimecodeFormat;
typedef uint32_t BMDVideoConnection;
typedef uint32_t BMDVideoInputConversionMode;
typedef uint32_t BMDSupportedVideoModeFlags;
typedef uint32_t BMDFieldDominance;
typedef uint32_t BMDDisplayModeFlags;
typedef uint32_t BMDColorspace;

const BMDDisplayMode bmdModeHD1080i5994 = 0x48693539;
const BMDPixelFormat bmdFormat8BitYUV = 0x32767579;
const BMDPixelFormat bmdFormat8BitBGRA = 0x42475241;
const BMDVideoInputFlags bmdVideoInputFlagDefault = 0x00000000;
const BMDVideoInputFlags bmdVideoInputEnableFormatDetection = 0x00000001;
const BMDAudioSampleRate bmdAudioSampleRate48kHz = 48000;
const BMDAudioSampleType bmdAudioSampleType16bitInteger = 16;
const BMDFrameFlags bmdFrameHasNoInputSource = 0x80000000;
const BMDDetectedVideoInputFormatFlags bmdDetectedVideoInputRGB444 = 0x00000002;
const BMDDeckLinkAttributeID BMDDeckLinkSupportsInputFormatDetection = 0x696E6664;
const BMDDeckLinkAttributeID BMDDeckLinkMaximumAudioChannels = 0x6D616368;

// Opaque interfaces we only pass as pointers.
struct IDeckLinkTimecode;
struct IDeckLinkVideoFrameAncillary;
struct IDeckLinkScreenPreviewCallback;
struct IDeckLinkVideoBufferAllocatorProvider;
struct IDeckLinkVideoBuffer;
struct IDeckLinkDisplayModeIterator;
struct IDeckLinkInputCallback;

struct IDeckLinkDisplayMode : public IUnknown {
    virtual HRESULT STDMETHODCALLTYPE GetName(BSTR* name) = 0;
    virtual BMDDisplayMode STDMETHODCALLTYPE GetDisplayMode() = 0;
    virtual int32_t STDMETHODCALLTYPE GetWidth() = 0;
    virtual int32_t STDMETHODCALLTYPE GetHeight() = 0;
    virtual HRESULT STDMETHODCALLTYPE GetFrameRate(int64_t* frameDuration, int64_t* timeScale) = 0;
    virtual BMDFieldDominance STDMETHODCALLTYPE GetFieldDominance() = 0;
    virtual BMDDisplayModeFlags STDMETHODCALLTYPE GetFlags() = 0;
};

// IDeckLinkVideoFrame: GetBytes reinserted after GetFlags (see file header).
struct IDeckLinkVideoFrame : public IUnknown {
    virtual int32_t STDMETHODCALLTYPE GetWidth() = 0;
    virtual int32_t STDMETHODCALLTYPE GetHeight() = 0;
    virtual int32_t STDMETHODCALLTYPE GetRowBytes() = 0;
    virtual BMDPixelFormat STDMETHODCALLTYPE GetPixelFormat() = 0;
    virtual BMDFrameFlags STDMETHODCALLTYPE GetFlags() = 0;
    virtual HRESULT STDMETHODCALLTYPE GetBytes(void** buffer) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetTimecode(BMDTimecodeFormat format, IDeckLinkTimecode** timecode) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetAncillaryData(IDeckLinkVideoFrameAncillary** ancillary) = 0;
};

struct IDeckLinkVideoInputFrame : public IDeckLinkVideoFrame {
    virtual HRESULT STDMETHODCALLTYPE GetStreamTime(int64_t* frameTime, int64_t* frameDuration, int64_t timeScale) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetHardwareReferenceTimestamp(int64_t timeScale, int64_t* frameTime, int64_t* frameDuration) = 0;
};

struct IDeckLinkAudioInputPacket : public IUnknown {
    virtual int32_t STDMETHODCALLTYPE GetSampleFrameCount() = 0;
    virtual HRESULT STDMETHODCALLTYPE GetBytes(void** buffer) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetPacketTime(int64_t* packetTime, int64_t timeScale) = 0;
};

struct IDeckLinkInputCallback : public IUnknown {
    virtual HRESULT STDMETHODCALLTYPE VideoInputFormatChanged(
        BMDVideoInputFormatChangedEvents notificationEvents,
        IDeckLinkDisplayMode* newDisplayMode,
        BMDDetectedVideoInputFormatFlags detectedSignalFlags) = 0;
    virtual HRESULT STDMETHODCALLTYPE VideoInputFrameArrived(
        IDeckLinkVideoInputFrame* videoFrame,
        IDeckLinkAudioInputPacket* audioPacket) = 0;
};

struct IDeckLinkInput : public IUnknown {
    virtual HRESULT STDMETHODCALLTYPE DoesSupportVideoMode(BMDVideoConnection, BMDDisplayMode, BMDPixelFormat,
        BMDVideoInputConversionMode, BMDSupportedVideoModeFlags, BMDDisplayMode*, int32_t*) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetDisplayMode(BMDDisplayMode, IDeckLinkDisplayMode**) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetDisplayModeIterator(IDeckLinkDisplayModeIterator**) = 0;
    virtual HRESULT STDMETHODCALLTYPE SetScreenPreviewCallback(IDeckLinkScreenPreviewCallback*) = 0;
    virtual HRESULT STDMETHODCALLTYPE EnableVideoInput(BMDDisplayMode, BMDPixelFormat, BMDVideoInputFlags) = 0;
    virtual HRESULT STDMETHODCALLTYPE EnableVideoInputWithAllocatorProvider(BMDDisplayMode, BMDPixelFormat,
        BMDVideoInputFlags, IDeckLinkVideoBufferAllocatorProvider*) = 0;
    virtual HRESULT STDMETHODCALLTYPE DisableVideoInput() = 0;
    virtual HRESULT STDMETHODCALLTYPE GetAvailableVideoFrameCount(unsigned*) = 0;
    virtual HRESULT STDMETHODCALLTYPE EnableAudioInput(BMDAudioSampleRate, BMDAudioSampleType, unsigned) = 0;
    virtual HRESULT STDMETHODCALLTYPE DisableAudioInput() = 0;
    virtual HRESULT STDMETHODCALLTYPE GetAvailableAudioSampleFrameCount(unsigned*) = 0;
    virtual HRESULT STDMETHODCALLTYPE StartStreams() = 0;
    virtual HRESULT STDMETHODCALLTYPE StopStreams() = 0;
    virtual HRESULT STDMETHODCALLTYPE PauseStreams() = 0;
    virtual HRESULT STDMETHODCALLTYPE FlushStreams() = 0;
    virtual HRESULT STDMETHODCALLTYPE SetCallback(IDeckLinkInputCallback*) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetHardwareReferenceClock(int64_t, int64_t*, int64_t*, int64_t*) = 0;
};

struct IDeckLinkProfileAttributes : public IUnknown {
    virtual HRESULT STDMETHODCALLTYPE GetFlag(BMDDeckLinkAttributeID, int32_t* value) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetInt(BMDDeckLinkAttributeID, int64_t* value) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetFloat(BMDDeckLinkAttributeID, double* value) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetString(BMDDeckLinkAttributeID, BSTR* value) = 0;
};

struct IDeckLink : public IUnknown {
    virtual HRESULT STDMETHODCALLTYPE GetModelName(BSTR* modelName) = 0;
    virtual HRESULT STDMETHODCALLTYPE GetDisplayName(BSTR* displayName) = 0;
};

struct IDeckLinkIterator : public IUnknown {
    virtual HRESULT STDMETHODCALLTYPE Next(IDeckLink** deckLinkInstance) = 0;
};

struct IDeckLinkVideoConversion : public IUnknown {
    virtual HRESULT STDMETHODCALLTYPE ConvertFrame(IDeckLinkVideoFrame* srcFrame, IDeckLinkVideoFrame* dstFrame) = 0;
    virtual HRESULT STDMETHODCALLTYPE ConvertNewFrame(IDeckLinkVideoFrame*, BMDPixelFormat, BMDColorspace,
        IDeckLinkVideoBuffer*, IDeckLinkVideoFrame**) = 0;
};

// CLSIDs / IIDs (from the installed driver's type library).
static const GUID CLSID_CDeckLinkIterator =
    {0xBA6C6F44, 0x6DA5, 0x4DCE, {0x94, 0xAA, 0xEE, 0x2D, 0x13, 0x72, 0xA6, 0x76}};
static const GUID CLSID_CDeckLinkVideoConversion =
    {0x89BA47BD, 0x1FE2, 0x4D76, {0x9B, 0xFE, 0xDE, 0x85, 0x04, 0x9C, 0x49, 0x87}};
static const GUID IID_IDeckLinkIterator =
    {0x50FB36CD, 0x3063, 0x4B73, {0xBD, 0xBB, 0x95, 0x80, 0x87, 0xF2, 0xD8, 0xBA}};
static const GUID IID_IDeckLinkInput =
    {0x4095DB82, 0xE294, 0x4B8C, {0xAA, 0xA8, 0x3B, 0x9E, 0x80, 0xC4, 0x93, 0x36}};
static const GUID IID_IDeckLinkProfileAttributes =
    {0x17D4BF8E, 0x4911, 0x473A, {0x80, 0xA0, 0x73, 0x1C, 0xF6, 0xFF, 0x34, 0x5B}};
static const GUID IID_IDeckLinkVideoConversion =
    {0xA48755D9, 0x8BD5, 0x4727, {0xA1, 0xE9, 0x06, 0x9F, 0xDE, 0xDB, 0xA6, 0xE9}};

#endif
