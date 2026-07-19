import { Subscribers } from './utils.js';
const imgElementId = 'uppy-image-editor-image';
export function createImageEditorController(uppy, options) {
    const plugin = uppy.getPlugin('ImageEditor');
    if (!plugin) {
        throw new Error('ImageEditor plugin is not installed. Install @uppy/image-editor and add it to the Uppy instance with `uppy.use(ImageEditor)`.');
    }
    const { file } = options;
    const subscribers = new Subscribers();
    const onStateUpdate = (_prev, _next, patch) => {
        const editorPatch = patch?.plugins?.ImageEditor;
        if (editorPatch) {
            subscribers.emit();
        }
    };
    const start = () => {
        uppy.on('state-update', onStateUpdate);
        plugin.start(file);
    };
    const stop = () => {
        uppy.off('state-update', onStateUpdate);
        plugin.stop();
    };
    const isCropperReady = () => plugin.getPluginState().cropperReady;
    // Actions
    const save = () => {
        plugin.save();
    };
    const cancel = () => {
        uppy.emit('file-editor:cancel', file);
    };
    const rotateBy = (degrees) => {
        plugin.rotateBy(degrees);
    };
    const rotateGranular = (degrees) => {
        plugin.rotateGranular(degrees);
    };
    const flipHorizontal = () => {
        plugin.flipHorizontal();
    };
    const zoom = (ratio) => {
        plugin.zoom(ratio);
    };
    const setAspectRatio = (newRatio) => {
        plugin.setAspectRatio(newRatio);
    };
    const reset = () => {
        plugin.reset();
    };
    // Props getters
    const getImageProps = () => ({
        id: imgElementId,
        src: plugin.getObjectUrl() ?? undefined,
        alt: file.name ?? '',
        onLoad: (e) => {
            plugin.initCropper(e.currentTarget);
        },
    });
    const getSaveButtonProps = (options = {}) => ({
        type: 'button',
        onClick: () => {
            save();
            options.onClick?.();
        },
        disabled: !isCropperReady(),
        'aria-label': 'Save changes',
    });
    const getCancelButtonProps = (options = {}) => ({
        type: 'button',
        onClick: () => {
            cancel();
            options.onClick?.();
        },
        disabled: false,
        'aria-label': 'Cancel editing',
    });
    const getRotateButtonProps = (degrees) => ({
        type: 'button',
        onClick: () => rotateBy(degrees),
        disabled: !isCropperReady(),
        'aria-label': `Rotate ${degrees} degrees`,
    });
    const getFlipHorizontalButtonProps = () => ({
        type: 'button',
        onClick: flipHorizontal,
        disabled: !isCropperReady(),
        'aria-label': 'Flip horizontally',
    });
    const getZoomButtonProps = (ratio) => ({
        type: 'button',
        onClick: () => zoom(ratio),
        disabled: !isCropperReady(),
        'aria-label': ratio > 0 ? 'Zoom in' : 'Zoom out',
    });
    const getCropSquareButtonProps = () => ({
        type: 'button',
        onClick: () => setAspectRatio('1:1'),
        disabled: !isCropperReady(),
        'aria-label': 'Crop square (1:1)',
    });
    const getCropLandscapeButtonProps = () => ({
        type: 'button',
        onClick: () => setAspectRatio('16:9'),
        disabled: !isCropperReady(),
        'aria-label': 'Crop landscape (16:9)',
    });
    const getCropPortraitButtonProps = () => ({
        type: 'button',
        onClick: () => setAspectRatio('9:16'),
        disabled: !isCropperReady(),
        'aria-label': 'Crop portrait (9:16)',
    });
    const getResetButtonProps = () => ({
        type: 'button',
        onClick: reset,
        disabled: !isCropperReady(),
        'aria-label': 'Reset all changes',
    });
    const getRotationSliderProps = () => ({
        type: 'range',
        min: -45,
        max: 45,
        value: plugin.getPluginState().angleGranular,
        onChange: (e) => {
            const granularAngle = Number(e.target.value);
            rotateGranular(granularAngle);
        },
        'aria-label': 'Fine rotation adjustment',
    });
    const getSnapshot = (pluginState = plugin.getPluginState()) => ({
        state: {
            angle: pluginState.angle,
            isFlippedHorizontally: pluginState.isFlippedHorizontally,
            aspectRatio: pluginState.aspectRatio,
        },
        getImageProps,
        getSaveButtonProps,
        getCancelButtonProps,
        getRotateButtonProps,
        getFlipHorizontalButtonProps,
        getZoomButtonProps,
        getCropSquareButtonProps,
        getCropLandscapeButtonProps,
        getCropPortraitButtonProps,
        getResetButtonProps,
        getRotationSliderProps,
    });
    let cachedPluginState = plugin.getPluginState();
    let cachedSnapshot = getSnapshot(cachedPluginState);
    const getCachedSnapshot = () => {
        const pluginState = plugin.getPluginState();
        if (pluginState === cachedPluginState)
            return cachedSnapshot;
        cachedPluginState = pluginState;
        cachedSnapshot = getSnapshot(pluginState);
        return cachedSnapshot;
    };
    return {
        subscribe: subscribers.add,
        getSnapshot: getCachedSnapshot,
        start,
        stop,
    };
}
