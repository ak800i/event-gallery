import { type ImageEditorSnapshot } from '@uppy/components';
import type { UppyFile } from '@uppy/core';
import { type ChangeEvent, type SyntheticEvent } from 'react';
type ImageEditorProps = {
    file: UppyFile<any, any>;
};
type ImageLoadEvent = Event & SyntheticEvent<HTMLImageElement>;
type SliderChangeEvent = Event & ChangeEvent<HTMLInputElement>;
export declare function useImageEditor(props: ImageEditorProps): ImageEditorSnapshot<ImageLoadEvent, SliderChangeEvent>;
export {};
//# sourceMappingURL=useImageEditor.d.ts.map