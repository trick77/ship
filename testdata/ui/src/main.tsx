import { createRoot } from "react-dom/client";
import { Greeting } from "./Greeting";

createRoot(document.getElementById("root")!).render(<Greeting name="ship" />);
