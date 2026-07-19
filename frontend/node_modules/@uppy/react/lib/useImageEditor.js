import { createImageEditorController, } from '@uppy/components';
import { useEffect, useMemo, useSyncExternalStore, } from 'react';
import { useUppyContext } from './headless/UppyContextProvider.js';
export function useImageEditor(props) {
    const { uppy } = useUppyContext();
    const controller = useMemo(() => createImageEditorController(uppy, {
        file: props.file,
    }), [uppy, props.file]);
    useEffect(() => {
        controller.start();
        return () => controller.stop();
    }, [controller]);
    const store = useSyncExternalStore(controller.subscribe, controller.getSnapshot, controller.getSnapshot);
    return store;
}
