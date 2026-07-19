import type { Uppy, UppyFile } from '@uppy/core';
import type { AspectRatio } from '@uppy/image-editor';
export type { AspectRatio } from '@uppy/image-editor';
export type ImageEditorState = {
    angle: number;
    isFlippedHorizontally: boolean;
    aspectRatio: AspectRatio;
};
type ButtonProps = {
    type: 'button';
    onClick: () => void;
    disabled: boolean;
    'aria-label': string;
};
type ButtonClickOptions = {
    onClick?: () => void;
};
type SliderProps<EventType extends Event = Event> = {
    type: 'range';
    min: number;
    max: number;
    value: number;
    onChange: (e: EventType) => void;
    'aria-label': string;
};
type ImageProps<EventType extends Event = Event> = {
    id: string;
    src: string | undefined;
    alt: string;
    onLoad: (e: EventType) => void;
};
export type ImageEditorSnapshot<ImageEventType extends Event = Event, SliderEventType extends Event = Event> = {
    state: ImageEditorState;
    getImageProps: () => ImageProps<ImageEventType>;
    getSaveButtonProps: (options?: ButtonClickOptions) => ButtonProps;
    getCancelButtonProps: (options?: ButtonClickOptions) => ButtonProps;
    getRotateButtonProps: (degrees: number) => ButtonProps;
    getFlipHorizontalButtonProps: () => ButtonProps;
    getZoomButtonProps: (ratio: number) => ButtonProps;
    getCropSquareButtonProps: () => ButtonProps;
    getCropLandscapeButtonProps: () => ButtonProps;
    getCropPortraitButtonProps: () => ButtonProps;
    getResetButtonProps: () => ButtonProps;
    getRotationSliderProps: () => SliderProps<SliderEventType>;
};
export type ImageEditorStore<ImageEventType extends Event = Event, SliderEventType extends Event = Event> = {
    subscribe: (listener: () => void) => () => void;
    getSnapshot: () => ImageEditorSnapshot<ImageEventType, SliderEventType>;
    start: () => void;
    stop: () => void;
};
export declare function createImageEditorController<ImageEventType extends Event = Event, SliderEventType extends Event = Event>(uppy: Uppy<any, any>, options: {
    file: UppyFile<any, any>;
}): ImageEditorStore<ImageEventType, SliderEventType>;
//# sourceMappingURL=image-editor.d.ts.map