import { DatasetsList } from "@/components/DatasetsList";

// The root URL is the dataset inventory — the analyst's data is the home of the
// console. The submission of goals lives on each dataset's detail view.
export default function HomePage() {
  return <DatasetsList />;
}